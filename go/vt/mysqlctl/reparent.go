// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package mysqlctl

/*
This file contains the reparenting methods for mysqlctl.

TODO(alainjobart) Once refactoring is done, remove unused code paths.
*/

import (
	"fmt"
	"time"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/vt/mysqlctl/proto"
	"golang.org/x/net/context"
)

// CreateReparentJournal returns the commands to execute to create
// the _vt.reparent_journal table. It is safe to run these commands
// even if the table already exists.
func CreateReparentJournal() []string {
	return []string{
		"CREATE DATABASE IF NOT EXISTS _vt",
		`CREATE TABLE IF NOT EXISTS _vt.reparent_journal (
  time_created_ns BIGINT UNSIGNED NOT NULL,
  action_name VARCHAR(250) NOT NULL,
  master_alias VARCHAR(32) NOT NULL,
  replication_position VARCHAR(250) DEFAULT NULL,
  PRIMARY KEY (time_created_ns)) ENGINE=InnoDB`}
}

// PopulateReparentJournal returns the SQL command to use to populate
// the _vt.reparent_journal table, as well as the time_created_ns
// value used.
func PopulateReparentJournal(timeCreatedNS int64, actionName, masterAlias string, pos proto.ReplicationPosition) string {
	return fmt.Sprintf("INSERT INTO _vt.reparent_journal "+
		"(time_created_ns, action_name, master_alias, replication_position) "+
		"VALUES (%v, '%v', '%v', '%v')",
		timeCreatedNS, actionName, masterAlias, proto.EncodeReplicationPosition(pos))
}

// queryReparentJournal returns the SQL query to use to query the database
// for a reparent_journal row.
func queryReparentJournal(timeCreatedNS int64) string {
	return fmt.Sprintf("SELECT action_name, master_alias, replication_position FROM _vt.reparent_journal WHERE time_created_ns=%v", timeCreatedNS)
}

// WaitForReparentJournal will wait until the context is done for
// the row in the reparent_journal table.
func (mysqld *Mysqld) WaitForReparentJournal(ctx context.Context, timeCreatedNS int64) error {
	for {
		qr, err := mysqld.fetchSuperQuery(queryReparentJournal(timeCreatedNS))
		if err == nil && len(qr.Rows) == 1 {
			// we have the row, we're done
			return nil
		}

		// wait a little bit, interrupt if context is done
		t := time.After(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t:
		}
	}
}

// DemoteMaster will gracefully demote a master mysql instance to read only.
// If the master is still alive, then we need to demote it gracefully
// make it read-only, flush the writes and get the position
func (mysqld *Mysqld) DemoteMaster() (rp proto.ReplicationPosition, err error) {
	// label as TYPE_REPLICA
	mysqld.SetReadOnly(true)
	cmds := []string{
		"FLUSH TABLES WITH READ LOCK",
		"UNLOCK TABLES",
	}
	if err = mysqld.ExecuteSuperQueryList(cmds); err != nil {
		return rp, err
	}
	return mysqld.MasterPosition()
}

// PromoteSlave will promote a mysql slave to master.
// setReadWrite: set the new master in read-write mode.
//
// replicationState: info slaves need to reparent themselves
// waitPosition: slaves can wait for this position when restarting replication
// timePromoted: this timestamp (unix nanoseconds) is inserted into _vt.replication_log to verify the replication config
func (mysqld *Mysqld) PromoteSlave(setReadWrite bool, hookExtraEnv map[string]string) (replicationStatus *proto.ReplicationStatus, waitPosition proto.ReplicationPosition, timePromoted int64, err error) {
	if err = mysqld.StopSlave(hookExtraEnv); err != nil {
		return
	}

	// If we are forced, we have to get our status as a master, not a slave.
	var lastRepPos proto.ReplicationPosition
	slaveStatus, err := mysqld.SlaveStatus()
	if err == ErrNotSlave {
		lastRepPos, err = mysqld.MasterPosition()
	} else {
		if err != nil {
			return
		}
		lastRepPos = slaveStatus.Position
	}

	// Promote to master.
	flavor, err := mysqld.flavor()
	if err != nil {
		err = fmt.Errorf("PromoteSlave needs flavor: %v", err)
		return
	}
	cmds := flavor.PromoteSlaveCommands()
	if err = mysqld.ExecuteSuperQueryList(cmds); err != nil {
		return
	}

	// Write a row so there's something in the binlog before we fetch the
	// master position. Otherwise, the slave may request a GTID that has
	// already been purged from the binlog.
	cmds = []string{
		fmt.Sprintf("INSERT INTO _vt.replication_log (time_created_ns, note) VALUES (%v, 'first binlog event')", time.Now().UnixNano()),
	}
	if err = mysqld.ExecuteSuperQueryList(cmds); err != nil {
		return
	}

	replicationPosition, err := mysqld.MasterPosition()
	if err != nil {
		return
	}
	mysqldAddr := mysqld.IPAddr()
	replicationStatus, err = proto.NewReplicationStatus(mysqldAddr)
	if err != nil {
		return
	}
	replicationStatus.Position = replicationPosition
	timePromoted = time.Now().UnixNano()
	// write a row to verify that replication is functioning
	cmds = []string{
		fmt.Sprintf("INSERT INTO _vt.replication_log (time_created_ns, note) VALUES (%v, 'reparent check')", timePromoted),
	}
	if err = mysqld.ExecuteSuperQueryList(cmds); err != nil {
		return
	}
	// this is the wait-point for checking replication
	waitPosition, err = mysqld.MasterPosition()
	if err != nil {
		return
	}
	if waitPosition.Equal(replicationPosition) {
		// We inserted a row, but our binlog position didn't change. This is a
		// serious problem. We don't want to ever promote a master like that.
		err = fmt.Errorf("cannot promote slave to master, non-functional binlogs")
		return
	}

	cmds = []string{
		fmt.Sprintf("INSERT INTO _vt.reparent_log (time_created_ns, last_position, new_addr, new_position, wait_position) VALUES (%v, '%v', '%v', '%v', '%v')",
			timePromoted, lastRepPos, replicationStatus.MasterAddr(), replicationPosition, waitPosition),
	}
	if err = mysqld.ExecuteSuperQueryList(cmds); err != nil {
		return
	}

	if setReadWrite {
		err = mysqld.SetReadOnly(false)
	}
	return
}

// RestartSlave tells a mysql slave that is has a new master
func (mysqld *Mysqld) RestartSlave(replicationStatus *proto.ReplicationStatus, waitPosition proto.ReplicationPosition, timeCheck int64) error {
	log.Infof("Restart Slave")
	cmds, err := mysqld.StartReplicationCommands(replicationStatus)
	if err != nil {
		return err
	}
	if err := mysqld.ExecuteSuperQueryList(cmds); err != nil {
		return err
	}

	if err := mysqld.WaitForSlaveStart(SlaveStartDeadline); err != nil {
		return err
	}

	if err := mysqld.WaitMasterPos(waitPosition, 0); err != nil {
		return err
	}

	return mysqld.CheckReplication(timeCheck)
}

// CheckReplication checks for the magic row inserted under controlled reparenting.
func (mysqld *Mysqld) CheckReplication(timeCheck int64) error {
	log.Infof("Check replication restarted")
	checkQuery := fmt.Sprintf("SELECT * FROM _vt.replication_log WHERE time_created_ns = %v",
		timeCheck)
	qr, err := mysqld.fetchSuperQuery(checkQuery)
	if err != nil {
		return err
	}
	if len(qr.Rows) != 1 {
		return fmt.Errorf("replication failed - unexpected row count %v", len(qr.Rows))
	}
	return nil
}
