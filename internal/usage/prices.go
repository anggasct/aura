package usage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	pricesFileVersion  = 1
	maxPricesFileBytes = 1 << 20
)

// pricesFile is the on-disk shape of the versioned operator price file. The
// schema is versioned so a future format change is a deliberate migration,
// not a silent misparse.
type pricesFile struct {
	Version int          `yaml:"version"`
	Prices  []priceEntry `yaml:"prices"`
}

type priceEntry struct {
	ModelDefinitionID       string `yaml:"model_definition_id"`
	Capability              string `yaml:"capability"`
	Currency                string `yaml:"currency"`
	MicrosPerInputToken     int64  `yaml:"micros_per_input_token"`
	MicrosPerOutputToken    int64  `yaml:"micros_per_output_token"`
	MicrosPerCacheToken     int64  `yaml:"micros_per_cache_token"`
	MicrosPerReasoningToken int64  `yaml:"micros_per_reasoning_token"`
	EffectiveFrom           string `yaml:"effective_from"`
	EffectiveTo             string `yaml:"effective_to"`
	Source                  string `yaml:"source"`
	MaxReservationRate      int64  `yaml:"max_reservation_rate"`
}

// LoadPricesFile reads a versioned operator price file into a registry. The
// effective timestamps are RFC3339; an empty effective_to means open-ended.
func LoadPricesFile(path string, reg *PriceRegistry) error {
	if reg == nil {
		return codedError(ErrorCodeInvalidArgument, "usage: price registry must not be nil", nil)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("usage: open price file: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxPricesFileBytes+1))
	if err != nil {
		return fmt.Errorf("usage: read price file: %w", err)
	}
	if len(data) > maxPricesFileBytes {
		return errors.New("usage: price file exceeds the size limit")
	}
	var doc pricesFile
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("usage: parse price file: %w", err)
	}
	if doc.Version != pricesFileVersion {
		return codedError(ErrorCodePriceVersionInvalid,
			fmt.Sprintf("usage: unsupported price file version %d (supported: %d)", doc.Version, pricesFileVersion), nil)
	}
	for i := range doc.Prices {
		p, err := parsePriceEntry(&doc.Prices[i])
		if err != nil {
			return fmt.Errorf("usage: price entry %d: %w", i, err)
		}
		if err := reg.Put(&p); err != nil {
			return fmt.Errorf("usage: price entry %d: %w", i, err)
		}
	}
	return nil
}

func parsePriceEntry(e *priceEntry) (Price, error) {
	from, err := time.Parse(time.RFC3339, e.EffectiveFrom)
	if err != nil {
		return Price{}, errors.New("effective_from must be RFC3339")
	}
	var to time.Time
	if e.EffectiveTo != "" {
		to, err = time.Parse(time.RFC3339, e.EffectiveTo)
		if err != nil {
			return Price{}, errors.New("effective_to must be RFC3339")
		}
		if !to.After(from) {
			return Price{}, errors.New("effective_to must be after effective_from")
		}
	}
	p := Price{
		ModelDefinitionID:       e.ModelDefinitionID,
		Capability:              e.Capability,
		Currency:                e.Currency,
		MicrosPerInputToken:     e.MicrosPerInputToken,
		MicrosPerOutputToken:    e.MicrosPerOutputToken,
		MicrosPerCacheToken:     e.MicrosPerCacheToken,
		MicrosPerReasoningToken: e.MicrosPerReasoningToken,
		EffectiveFrom:           from,
		EffectiveTo:             to,
		Source:                  e.Source,
		MaxReservationRate:      e.MaxReservationRate,
	}
	if !p.valid() {
		return Price{}, errors.New("invalid price record")
	}
	return p, nil
}
