package operationtest

import (
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

func faultClass(err error) (toolbroker.ResultClass, bool) {
	return fault.CodeOf[toolbroker.ResultClass](err)
}
