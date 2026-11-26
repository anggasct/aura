package store

type rowScanner interface {
	Scan(dest ...any) error
}
