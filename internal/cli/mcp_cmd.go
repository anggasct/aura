package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/mcp"
)

const (
	mcpTrustDirName  = "mcp"
	mcpTrustFileName = "trust.json"
	mcpRestartNotice = "takes effect at the next server start"
)

func mcpTrustRegistryPath(cfg *config.Config) (string, error) {
	dataRoot, err := resolveDataRoot(cfg)
	if err != nil {
		return "", err
	}
	return filepath.Join(dataRoot, mcpTrustDirName, mcpTrustFileName), nil
}

func configuredMCPServers(cfg *config.Config) []config.MCPServer {
	if cfg == nil || cfg.MCP == nil {
		return nil
	}
	return cfg.MCP.Servers
}

func configuredMCPServerNames(cfg *config.Config) []string {
	servers := configuredMCPServers(cfg)
	names := make([]string, 0, len(servers))
	for i := range servers {
		names = append(names, servers[i].Name)
	}
	return names
}

func newMCPCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Review configured MCP servers and their trust state",
	}
	trust := &cobra.Command{
		Use:   "trust",
		Short: "Approve or revoke trust for a configured MCP server",
	}
	trust.AddCommand(newMCPTrustApproveCmd(gf), newMCPTrustRevokeCmd(gf))
	cmd.AddCommand(newMCPListCmd(gf), trust)
	return cmd
}

type mcpListRow struct {
	Name         string   `json:"name"`
	Transport    string   `json:"transport"`
	State        string   `json:"state"`
	DigestPrefix string   `json:"digest_prefix"`
	Tools        []string `json:"tools"`
}

type mcpListOutput struct {
	Servers []mcpListRow `json:"servers"`
}

func newMCPListCmd(gf *globalFlags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured MCP servers with their trust state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			registry, err := openMCPTrustRegistry(result.Config)
			if err != nil {
				return err
			}
			reviews, err := mcp.ReviewServers(cmd.Context(), registry, configuredMCPServers(result.Config))
			if err != nil {
				return err
			}
			rows := make([]mcpListRow, 0, len(reviews))
			for i := range reviews {
				rows = append(rows, mcpListRow{
					Name:         reviews[i].Name,
					Transport:    reviews[i].Transport,
					State:        string(reviews[i].State),
					DigestPrefix: reviews[i].DigestPrefix(),
					Tools:        append([]string{}, reviews[i].Tools...),
				})
			}
			out := cmd.OutOrStdout()
			if asJSON {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(mcpListOutput{Servers: rows})
			}
			if len(rows) == 0 {
				_, err := fmt.Fprintln(out, "no mcp servers configured")
				return err
			}
			if _, err := fmt.Fprintln(out, "SERVER\tTRANSPORT\tSTATE\tDIGEST\tTOOLS"); err != nil {
				return err
			}
			for i := range rows {
				if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", rows[i].Name, rows[i].Transport, rows[i].State, rows[i].DigestPrefix, toolsColumn(rows[i].Tools)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable output")
	return cmd
}

func toolsColumn(tools []string) string {
	if len(tools) == 0 {
		return "-"
	}
	return strings.Join(tools, ",")
}

func openMCPTrustRegistry(cfg *config.Config) (*mcp.FileTrustRegistry, error) {
	path, err := mcpTrustRegistryPath(cfg)
	if err != nil {
		return nil, err
	}
	return mcp.NewFileTrustRegistry(path)
}

func findMCPServer(cfg *config.Config, name string) (*config.MCPServer, bool) {
	servers := configuredMCPServers(cfg)
	for i := range servers {
		if servers[i].Name == name {
			return &servers[i], true
		}
	}
	return nil, false
}

func newMCPTrustApproveCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "approve <server>",
		Short: "Approve the current configuration of an MCP server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			server, ok := findMCPServer(result.Config, args[0])
			if !ok {
				return mcp.Errorf(mcp.ErrConfigInvalid, "server %q is not configured", args[0])
			}
			registry, err := openMCPTrustRegistry(result.Config)
			if err != nil {
				return err
			}
			approval, err := mcp.ApproveServer(cmd.Context(), registry, server)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			digest := approval.Review.DigestPrefix()
			switch approval.Change {
			case mcp.ApprovalChangeAlreadyCurrent:
				_, err = fmt.Fprintf(out, "%s is already approved (digest %s); no change\n", server.Name, digest)
			case mcp.ApprovalChangeToolSurface:
				_, err = fmt.Fprintf(out, "approved the changed tool surface of %s (%d tools: %s, digest %s); %s\n", server.Name, len(approval.Tools), strings.Join(approval.Tools, ", "), digest, mcpRestartNotice)
			default:
				_, err = fmt.Fprintf(out, "approved %s (%s, digest %s); %s\n", server.Name, server.Transport, digest, mcpRestartNotice)
			}
			return err
		},
	}
}

func newMCPTrustRevokeCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <server>",
		Short: "Remove the trust record of an MCP server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			registry, err := openMCPTrustRegistry(result.Config)
			if err != nil {
				return err
			}
			removed, err := mcp.RevokeServer(cmd.Context(), registry, args[0])
			if err != nil {
				return err
			}
			_, configured := findMCPServer(result.Config, args[0])
			if !removed && !configured {
				return mcp.Errorf(mcp.ErrConfigInvalid, "server %q is neither configured nor trusted", args[0])
			}
			if !removed {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s had no trust record; no change\n", args[0])
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "revoked trust for %s; %s\n", args[0], mcpRestartNotice)
			return err
		},
	}
}
