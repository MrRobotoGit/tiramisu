package settings

import (
	"tiramisu/internal/gostorm/log"
	"tiramisu/internal/metadb"
)

// stateDB is the process state DB (tiramisu.db); main opens it before server.Start.
var stateDB *metadb.DB

// SetStateDB hands GoStorm the state DB. Must run before InitSets.
func SetStateDB(db *metadb.DB) { stateDB = db }

// SQLiteDB is the GoStormDB backend on gostorm_kv, in place of BBolt.
type SQLiteDB struct {
	db *metadb.DB
}

func NewSQLiteDB(db *metadb.DB) GoStormDB {
	if db == nil {
		return nil
	}
	return &SQLiteDB{db: db}
}

// CloseDB is a no-op: the DB belongs to the process, not to GoStorm.
func (v *SQLiteDB) CloseDB() {}

func (v *SQLiteDB) Get(xPath, name string) []byte {
	b, err := v.db.GoStormKVGet(xPath, name)
	if err != nil {
		log.TLogln("Error get sets", xPath+"/"+name, ", error:", err)
	}
	return b
}

func (v *SQLiteDB) Set(xPath, name string, value []byte) {
	if err := v.db.GoStormKVSet(xPath, name, value); err != nil {
		log.TLogln("Error put sets", xPath+"/"+name, ", error:", err)
	}
}

func (v *SQLiteDB) List(xPath string) []string {
	l, err := v.db.GoStormKVList(xPath)
	if err != nil {
		log.TLogln("Error list sets", xPath, ", error:", err)
	}
	return l
}

func (v *SQLiteDB) Rem(xPath, name string) {
	if err := v.db.GoStormKVDelete(xPath, name); err != nil {
		log.TLogln("Error rem sets", xPath+"/"+name, ", error:", err)
	}
}

func (v *SQLiteDB) Clear(xPath string) {
	if err := v.db.GoStormKVClear(xPath); err != nil {
		log.TLogln("Error clear xPath", xPath, ", error:", err)
	}
}
