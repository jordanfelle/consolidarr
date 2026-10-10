package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// KeyPrefix marks consolidarr API keys so they are recognisable in configs and secret scanners.
const KeyPrefix = "csl_"

type APIKey struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Prefix   string `json:"prefix"` // first characters only, to tell keys apart in the UI
	Created  int64  `json:"created"`
	LastUsed int64  `json:"lastUsed"` // 0 = never
}

func hashKey(k string) string { h := sha256.Sum256([]byte(k)); return hex.EncodeToString(h[:]) }

// CreateKey returns the plaintext key exactly once; only its SHA-256 is stored.
func (s *Store) CreateKey(name string) (APIKey, string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return APIKey{}, "", err
	}
	plain := KeyPrefix + hex.EncodeToString(b)
	k := APIKey{Name: name, Prefix: plain[:len(KeyPrefix)+6], Created: time.Now().Unix()}
	err := s.QueryRow(`insert into api_keys(name,prefix,hash,created) values(?,?,?,?) returning id`, k.Name, k.Prefix, hashKey(plain), k.Created).Scan(&k.ID)
	return k, plain, err
}

func (s *Store) Keys() ([]APIKey, error) {
	rows, err := s.Query(`select id,name,prefix,created,last_used from api_keys order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Created, &k.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) DeleteKey(id int64) error {
	res, err := s.Exec(`delete from api_keys where id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// KeyValid reports whether plain is a live key, and records its use (at most once a minute).
func (s *Store) KeyValid(plain string) bool {
	if len(plain) < len(KeyPrefix) || plain[:len(KeyPrefix)] != KeyPrefix {
		return false
	}
	var id, last int64
	if s.QueryRow(`select id,last_used from api_keys where hash=?`, hashKey(plain)).Scan(&id, &last) != nil {
		return false
	}
	if now := time.Now().Unix(); now-last > 60 {
		_, _ = s.Exec(`update api_keys set last_used=? where id=?`, now, id)
	}
	return true
}
