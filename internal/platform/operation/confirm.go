package operation

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
)

const (
	ConfirmKeyFile  = "operation-confirm.key"
	confirmKeyBytes = 32
	confirmCodeBits = 40
	crockford       = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	codeGroupSize   = 4
)

type Confirmer struct {
	dir string
	mu  sync.Mutex
	key []byte
}

func NewConfirmer(dir string) (*Confirmer, error) {
	if dir == "" {
		return nil, nilArgument("confirmation key directory")
	}
	return &Confirmer{dir: dir}, nil
}

func (c *Confirmer) Code(digest string) (string, error) {
	if c == nil {
		return "", nilArgument("confirmer")
	}
	key, err := c.loadKey()
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(digest))
	return encodeCode(mac.Sum(nil)[:confirmCodeBits/8]), nil
}

func (c *Confirmer) Matches(digest, supplied string) bool {
	expected, err := c.Code(digest)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(normalizeCode(expected)), []byte(normalizeCode(supplied)))
}

func encodeCode(raw []byte) string {
	var bits uint64
	for _, b := range raw {
		bits = bits<<8 | uint64(b)
	}
	groups := confirmCodeBits / 5
	var out strings.Builder
	for i := range groups {
		shift := uint(5 * (groups - 1 - i))
		out.WriteByte(crockford[(bits>>shift)&31])
		if (i+1)%codeGroupSize == 0 && i+1 < groups {
			out.WriteByte('-')
		}
	}
	return out.String()
}

func normalizeCode(code string) string {
	var out strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch r {
		case '-', ' ':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		out.WriteRune(r)
	}
	return out.String()
}

func (c *Confirmer) loadKey() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key != nil {
		return c.key, nil
	}
	root, err := os.OpenRoot(c.dir)
	if err != nil {
		return nil, wrapError(ErrorCodeFailed, "the confirmation key directory cannot be opened", err)
	}
	defer func() { _ = root.Close() }()
	key, err := readOrCreateKey(root)
	if err != nil {
		return nil, err
	}
	c.key = key
	return key, nil
}

func readOrCreateKey(root *os.Root) ([]byte, error) {
	key, err := readKey(root)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	if err := createKey(root); err != nil {
		return nil, err
	}
	return readKey(root)
}

func readKey(root *os.Root) ([]byte, error) {
	file, err := root.Open(ConfirmKeyFile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, wrapError(ErrorCodeFailed, "the confirmation key cannot be read", err)
	}
	if info.Mode().Perm()&0o077 != 0 || info.Size() != confirmKeyBytes {
		return nil, NewError(ErrorCodeFailed, "the confirmation key file is not private or not the expected size")
	}
	key := make([]byte, confirmKeyBytes)
	if _, err := io.ReadFull(file, key); err != nil {
		return nil, wrapError(ErrorCodeFailed, "the confirmation key cannot be read", err)
	}
	return key, nil
}

func createKey(root *os.Root) error {
	key := make([]byte, confirmKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return wrapError(ErrorCodeFailed, "the confirmation key cannot be generated", err)
	}
	temp := fmt.Sprintf("%s.%x.tmp", ConfirmKeyFile, key[:6])
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return wrapError(ErrorCodeFailed, "the confirmation key cannot be created", err)
	}
	defer func() { _ = root.Remove(temp) }()
	if _, err := file.Write(key); err != nil {
		_ = file.Close()
		return wrapError(ErrorCodeFailed, "the confirmation key cannot be written", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return wrapError(ErrorCodeFailed, "the confirmation key cannot be synced", err)
	}
	if err := file.Close(); err != nil {
		return wrapError(ErrorCodeFailed, "the confirmation key cannot be closed", err)
	}
	if err := root.Link(temp, ConfirmKeyFile); err != nil && !errors.Is(err, fs.ErrExist) {
		return wrapError(ErrorCodeFailed, "the confirmation key cannot be stored", err)
	}
	return syncRoot(root)
}

func syncRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return wrapError(ErrorCodeFailed, "the confirmation key directory cannot be synced", err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return wrapError(ErrorCodeFailed, "the confirmation key directory cannot be synced", err)
	}
	return nil
}
