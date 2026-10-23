// Package minidb is a tiny file-backed table store standing in for a real
// database in the demo service. Tables are JSON files on disk; a query
// returns rows as string maps. Its only job is to be a realistic DB hop
// whose returned rows can be captured into a span payload.
package minidb

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Table is a named set of rows with a fixed column order.
type Table struct {
	Name    string              `json:"name"`
	Columns []string            `json:"columns"`
	Rows    []map[string]string `json:"rows"`
}

// DB is a directory of table files.
type DB struct {
	dir    string
	tables map[string]*Table
}

// Create writes a fresh database directory from the given tables.
func Create(dir string, tables []*Table) (*DB, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db := &DB{dir: dir, tables: map[string]*Table{}}
	for _, t := range tables {
		raw, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, t.Name+".json"), raw, 0o644); err != nil {
			return nil, err
		}
		db.tables[t.Name] = t
	}
	return db, nil
}

// Open loads an existing database directory.
func Open(dir string) (*DB, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	db := &DB{dir: dir, tables: map[string]*Table{}}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var t Table
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("minidb: table %s: %w", e.Name(), err)
		}
		db.tables[t.Name] = &t
	}
	return db, nil
}

// GetByID returns a copy of the row whose "id" column equals id.
func (db *DB) GetByID(table, id string) (map[string]string, error) {
	t, ok := db.tables[table]
	if !ok {
		return nil, fmt.Errorf("minidb: no table %q", table)
	}
	for _, r := range t.Rows {
		if r["id"] == id {
			cp := make(map[string]string, len(r))
			for k, v := range r {
				cp[k] = v
			}
			return cp, nil
		}
	}
	return nil, fmt.Errorf("minidb: %s: no row id=%s", table, id)
}

// Columns returns the column order of a table.
func (db *DB) Columns(table string) []string {
	if t, ok := db.tables[table]; ok {
		return t.Columns
	}
	return nil
}

// RowsHash is the canonical hash of a row set: rows sorted by id, fields in
// column order, sha256 over the joined encoding. Used in DB span payloads
// so a reader can verify row integrity at a glance.
func RowsHash(columns []string, rows []map[string]string) string {
	sorted := make([]map[string]string, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i]["id"] < sorted[j]["id"] })
	var b strings.Builder
	for _, r := range sorted {
		for _, c := range columns {
			b.WriteString(c)
			b.WriteByte('=')
			b.WriteString(r[c])
			b.WriteByte(';')
		}
		b.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
