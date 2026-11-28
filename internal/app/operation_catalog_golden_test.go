package app

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/operation"
)

func operationCatalogLines(catalogs []operation.Catalog) []string {
	var lines []string
	for i := range catalogs {
		for j := range catalogs[i].Descriptors {
			lines = append(lines, operationCatalogLine(&catalogs[i].Descriptors[j]))
		}
	}
	slices.Sort(lines)
	return lines
}

func operationCatalogLine(d *operation.Descriptor) string {
	var surfaces []string
	for _, surface := range []operation.Surface{operation.SurfaceCLI, operation.SurfaceChat, operation.SurfaceAgent} {
		if d.Projects(surface) {
			surfaces = append(surfaces, string(surface))
		}
	}
	var secrets []string
	for i := range d.Params {
		if d.Params[i].Secret {
			secrets = append(secrets, "in:"+d.Params[i].Name)
		}
	}
	for i := range d.Output.Fields {
		if d.Output.Fields[i].Secret {
			secrets = append(secrets, "out:"+d.Output.Fields[i].Name)
		}
	}
	tool := "-"
	switch {
	case d.ModuleTool != "":
		tool = d.ModuleTool
	case d.GeneratesTool():
		tool = operation.ToolName(d.ID)
	}
	return fmt.Sprintf("%s exposure=%s risk=%s commit=%s fresh=%s notice=%s sink=%s untrusted=%s preview=%s confirm=%s network=%s grant=%s surfaces=%s secret=%s caps=%s tool=%s",
		d.ID, d.Exposure, d.Risk, d.Commit,
		strconv.FormatBool(d.FreshApproval), strconv.FormatBool(d.OwnerNotice), strconv.FormatBool(d.Sink),
		strconv.FormatBool(d.UntrustedOutput), strconv.FormatBool(d.Preview), joinOrDash([]string{string(d.OwnerConfirmation())}), strconv.FormatBool(d.ReachesNetwork),
		joinOrDash([]string{string(d.TaintedGrant)}), joinOrDash(surfaces), joinOrDash(secrets), joinOrDash(d.RequiredCapabilities), tool)
}

func joinOrDash(values []string) string {
	if len(values) == 0 || len(values) == 1 && values[0] == "" {
		return "-"
	}
	return strings.Join(values, ",")
}

func TestOperationCatalogGolden(t *testing.T) {
	compareAppGolden(t, "operation-catalog.txt", strings.Join(operationCatalogLines(operationCatalogs()), "\n")+"\n")
}
