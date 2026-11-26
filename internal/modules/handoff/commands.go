package handoff

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	ActionReply   = "handoff_reply"
	ActionRelease = "handoff_release"
)

type commandUsageError struct {
	err error
}

func (e *commandUsageError) Error() string { return e.err.Error() }

func (e *commandUsageError) Unwrap() error { return e.err }

func (e *commandUsageError) IsUsageError() bool { return true }

type CommandDeps struct {
	Store         Store
	Deliver       func(ctx context.Context, handoff *Handoff, text string) error
	PersonaAuthor func(agentID string) string
	ClosedMessage func(agentID string) string
	AppendReply   func(ctx context.Context, handoff *Handoff, author, text string, at time.Time) error
	RecordAction  func(ctx context.Context, action string, handoff *Handoff, eventID string, at time.Time) error
	Logger        *slog.Logger
	Now           func() time.Time
}

type CommandOpener func(cmd *cobra.Command, fn func(ctx context.Context, deps *CommandDeps) error) error

func Commands(open CommandOpener) []*cobra.Command {
	return []*cobra.Command{replyCommand(open), handoffCommand(open)}
}

func runWithOpener(cmd *cobra.Command, open CommandOpener, fn func(ctx context.Context, deps *CommandDeps) error) error {
	if open == nil {
		return fault.New(ErrorCodeInvalidArgument, "handoff command opener must not be nil")
	}
	return open(cmd, fn)
}

func openHandoffRecord(ctx context.Context, store Store, sessionID string) (Handoff, error) {
	if store == nil {
		return Handoff{}, fault.New(ErrorCodeInvalidArgument, "handoff store must not be nil")
	}
	handoff, found, err := store.Get(ctx, sessionID)
	if err != nil {
		return Handoff{}, err
	}
	if !found || !handoff.ReleasedAt.IsZero() {
		return Handoff{}, fault.New(ErrorCodeNotOpen, "this session has no open handoff")
	}
	return handoff, nil
}

func writeLines(cmd *cobra.Command, lines []string) error {
	out := cmd.OutOrStdout()
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

func currentTime(now func() time.Time) time.Time {
	if now == nil {
		return time.Now().UTC()
	}
	return now().UTC()
}

func replyCommand(open CommandOpener) *cobra.Command {
	return &cobra.Command{
		Use:   "reply <session> <text>",
		Short: "Reply as the persona in a handed-off session",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := strings.TrimSpace(strings.Join(args[1:], " "))
			if text == "" {
				return &commandUsageError{err: errors.New("reply text must not be empty")}
			}
			return runWithOpener(cmd, open, func(ctx context.Context, deps *CommandDeps) error {
				if deps == nil {
					return fault.New(ErrorCodeInvalidArgument, "handoff command dependencies must not be nil")
				}
				handoff, err := openHandoffRecord(ctx, deps.Store, args[0])
				if err != nil {
					return err
				}
				if deps.Deliver != nil {
					if err := deps.Deliver(ctx, &handoff, text); err != nil {
						return err
					}
				}
				now := currentTime(deps.Now)
				author := "aura"
				if deps.PersonaAuthor != nil {
					if resolved := deps.PersonaAuthor(handoff.AgentID); resolved != "" {
						author = resolved
					}
				}
				if deps.AppendReply != nil {
					if err := deps.AppendReply(ctx, &handoff, author, text, now); err != nil {
						return err
					}
				}
				if deps.RecordAction != nil {
					if err := deps.RecordAction(ctx, ActionReply, &handoff, "reply-"+handoff.SessionID+"-"+now.Format(time.RFC3339Nano), now); err != nil {
						return err
					}
				}
				return writeLines(cmd, []string{"replied in " + handoff.SessionID})
			})
		},
	}
}

func handoffCommand(open CommandOpener) *cobra.Command {
	cmd := &cobra.Command{Use: "handoff", Short: "Inspect and release sessions handed off to the owner"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List open handoffs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, deps *CommandDeps) error {
				if deps == nil || deps.Store == nil {
					return fault.New(ErrorCodeInvalidArgument, "handoff store must not be nil")
				}
				openHandoffs, err := deps.Store.ListOpen(ctx)
				if err != nil {
					return err
				}
				if len(openHandoffs) == 0 {
					return writeLines(cmd, []string{"no open handoffs"})
				}
				lines := []string{"SESSION\tPRINCIPAL\tPERSONA\tOPENED\tREASON"}
				for i := range openHandoffs {
					lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", openHandoffs[i].SessionID, openHandoffs[i].PrincipalID, openHandoffs[i].AgentID, openHandoffs[i].OpenedAt.UTC().Format(time.RFC3339), openHandoffs[i].Reason))
				}
				return writeLines(cmd, lines)
			})
		},
	}, &cobra.Command{
		Use:   "release <session>",
		Short: "Hand the conversation back to the persona",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, deps *CommandDeps) error {
				if deps == nil || deps.Store == nil {
					return fault.New(ErrorCodeInvalidArgument, "handoff store must not be nil")
				}
				if _, err := openHandoffRecord(ctx, deps.Store, args[0]); err != nil {
					return err
				}
				now := currentTime(deps.Now)
				handoff, released, err := deps.Store.Release(ctx, args[0], now)
				if err != nil {
					return err
				}
				if !released {
					return fault.New(ErrorCodeNotOpen, "this session has no open handoff")
				}
				if deps.Deliver != nil && deps.ClosedMessage != nil {
					if err := deps.Deliver(ctx, &handoff, deps.ClosedMessage(handoff.AgentID)); err != nil {
						logger := deps.Logger
						if logger == nil {
							logger = slog.Default()
						}
						logger.WarnContext(ctx, "handoff release notice failed", "component", "access")
					}
				}
				if deps.RecordAction != nil {
					if err := deps.RecordAction(ctx, ActionRelease, &handoff, "release-"+handoff.SessionID+"-"+now.Format(time.RFC3339Nano), now); err != nil {
						return err
					}
				}
				return writeLines(cmd, []string{"released " + handoff.SessionID})
			})
		},
	})
	return cmd
}
