package embedding

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"

	"github.com/anggasct/aura/internal/config"
)

func Fingerprint(cfg *config.KnowledgeEmbedding) (string, error) {
	endpoint, err := cfg.Endpoint()
	if err != nil {
		return "", Errorf(ErrorCodeConfigInvalid, "%v", err)
	}
	digest := sha256.New()
	for _, field := range []string{
		endpoint.Scheme, endpoint.Host, endpoint.Port, endpoint.Path,
		cfg.Model, strconv.Itoa(cfg.Dimensions), cfg.DimensionsField, cfg.RoleStyle,
		cfg.QueryPrefix, cfg.DocumentPrefix,
	} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		digest.Write(length[:])
		digest.Write([]byte(field))
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
