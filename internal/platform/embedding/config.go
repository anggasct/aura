package embedding

const (
	RoleStyleNone      = "none"
	RoleStyleInputType = "input_type"
	RoleStylePrefix    = "prefix"

	MaxBatchSize = 1000
)

type Config struct {
	BaseURL         string
	Model           string
	Dimensions      int
	DimensionsField string
	RoleStyle       string
	QueryPrefix     string
	DocumentPrefix  string
}
