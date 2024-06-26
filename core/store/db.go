package store

import (
	"github.com/flarco/g"
	"github.com/jmoiron/sqlx"
	"github.com/slingdata-io/sling-cli/core/dbio/database"
	"github.com/slingdata-io/sling-cli/core/env"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	// Db is the main databse connection
	Db   *gorm.DB
	Dbx  *sqlx.DB
	Conn database.Connection

	// DropAll signifies to drop all tables and recreate them
	DropAll = false
)

// InitDB initializes the database
func InitDB() {
	var err error

	if Db != nil {
		// already initiated
		return
	}

	dbURL := g.F("sqlite://%s/.sling.db?cache=shared&mode=rwc&_journal_mode=WAL", env.HomeDir)
	Conn, err = database.NewConn(dbURL, "silent=true")
	if err != nil {
		g.Debug("could not initialize local .sling.db. %s", err.Error())
		return
	}

	err = Conn.Connect()
	if err != nil {
		g.Debug("could not connect to local .sling.db. %s", err.Error())
		return
	}

	Db, err = Conn.GetGormConn(&gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		g.Debug("could not connect to local .sling.db. %s", err.Error())
		return
	}

	allTables := []interface{}{
		&Execution{},
		&Task{},
		&Replication{},
	}

	// manual migrations
	migrate()

	for _, table := range allTables {
		dryDB := Db.Session(&gorm.Session{DryRun: true})
		tableName := dryDB.Find(table).Statement.Table
		if DropAll {
			Db.Exec(g.F(`drop table if exists "%s"`, tableName))
		}
		err = Db.AutoMigrate(table)
		if err != nil {
			g.Debug("error AutoMigrating table for local .sling.db. => %s\n%s", tableName, err.Error())
			return
		}
	}
}

func migrate() {
	// migrations
	Db.Migrator().RenameColumn(&Task{}, "task", "config")               // rename column for consistency
	Db.Migrator().RenameColumn(&Replication{}, "replication", "config") // rename column for consistency

	// fix bad unique index on Execution.ExecID
	data, _ := Conn.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND sql LIKE '%UNIQUE%' /* nD */`)
	if len(data.Rows) > 0 {
		Db.Exec(g.F("drop index if exists %s", data.Rows[0][0]))
	}
}
