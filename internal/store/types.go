package store

import "database/sql"

type rowScanner interface {
	Scan(dest ...any) error
}

func nullText(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}
