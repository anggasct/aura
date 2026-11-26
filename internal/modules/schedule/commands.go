package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/fault"
)

type cliUsageError struct {
	err error
}

func (e *cliUsageError) Error() string { return e.err.Error() }

func (e *cliUsageError) Unwrap() error { return e.err }

func (e *cliUsageError) UsageError() bool { return true }

type CommandEnv struct {
	Config          Config
	Store           *SQLiteStore
	Service         *Service
	Starter         Starter
	WorkflowChecker func(ctx context.Context, id string) error
}

type Opener func(cmd *cobra.Command, needDurable bool, fn func(ctx context.Context, env *CommandEnv) error) error

func Commands(open Opener) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "cron",
		Short: "Manage scheduled recurring jobs",
	}
	cmd.AddCommand(
		newAddCmd(open),
		newListCmd(open),
		newPauseCmd(open),
		newResumeCmd(open),
		newDeleteCmd(open),
		newRunsCmd(open),
		newRunNowCmd(open),
	)
	return []*cobra.Command{cmd}
}

func exactOneJobID(cmd *cobra.Command, args []string) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return &cliUsageError{fmt.Errorf("%s requires exactly one job ID", cmd.CommandPath())}
	}
	return nil
}

func requireOpener(open Opener) error {
	if open == nil {
		return fault.New(ErrorCodeInvalidArgument, "schedule command opener must not be nil")
	}
	return nil
}

func validateWorkflowInputJSON(input string) error {
	if input == "" {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.DisallowUnknownFields()
	var parsed struct {
		Objective string `json:"objective"`
	}
	if err := decoder.Decode(&parsed); err != nil {
		return &cliUsageError{fmt.Errorf("--input must be a JSON object with an optional objective: %w", err)}
	}
	return nil
}

func newAddCmd(open Opener) *cobra.Command {
	var name, cronSchedule, timezone, channel, destination, prompt, overlap, workflowID, workflowInput string
	var grace time.Duration
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Create a scheduled job and start its run",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOpener(open); err != nil {
				return err
			}
			return open(cmd, true, func(ctx context.Context, env *CommandEnv) error {
				if env == nil || env.Service == nil || env.Starter == nil {
					return fault.New(ErrorCodeInvalidArgument, "schedule command environment is incomplete")
				}
				tz := timezone
				if tz == "" {
					tz = env.Config.DefaultTimezone
				}
				graceSeconds := int64(grace.Seconds())
				if !cmd.Flags().Changed("grace") {
					graceSeconds = int64(time.Duration(env.Config.DefaultCatchUpGrace).Seconds())
				}
				for _, flag := range []struct {
					name  string
					value string
				}{
					{"name", name},
					{"schedule", cronSchedule},
					{"channel", channel},
					{"destination", destination},
				} {
					if flag.value == "" {
						return &cliUsageError{fmt.Errorf("%s requires --%s", cmd.CommandPath(), flag.name)}
					}
				}
				if (prompt == "") == (workflowID == "") {
					return &cliUsageError{fmt.Errorf("%s requires exactly one of --prompt or --workflow", cmd.CommandPath())}
				}
				if workflowInput != "" && workflowID == "" {
					return &cliUsageError{fmt.Errorf("%s --input requires --workflow", cmd.CommandPath())}
				}
				if workflowID != "" {
					if err := validateWorkflowInputJSON(workflowInput); err != nil {
						return err
					}
					if env.WorkflowChecker == nil {
						return fault.New(ErrorCodeInvalidArgument, "workflow checker must not be nil")
					}
					if err := env.WorkflowChecker(ctx, workflowID); err != nil {
						return err
					}
				}
				job, err := env.Service.Create(ctx, &JobSpec{
					Name: name, CronExpression: cronSchedule, Timezone: tz,
					Prompt: prompt, OriginChannel: channel, OriginDestination: destination,
					OverlapPolicy: overlap, CatchUpGraceSeconds: graceSeconds,
					WorkflowID: workflowID, WorkflowInput: workflowInput,
				})
				if err != nil {
					return err
				}
				if err := StartNext(ctx, &NextRequest{Starter: env.Starter, Job: &job, After: time.Now().UTC()}); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "id: %s\n", job.ID)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "job name")
	cmd.Flags().StringVar(&cronSchedule, "schedule", "", "five-field cron expression")
	cmd.Flags().StringVar(&timezone, "timezone", "", "IANA timezone (default from config)")
	cmd.Flags().StringVar(&channel, "channel", "", "origin channel source")
	cmd.Flags().StringVar(&destination, "destination", "", "origin destination alias")
	cmd.Flags().StringVar(&prompt, "prompt", "", "turn prompt")
	cmd.Flags().StringVar(&workflowID, "workflow", "", "workflow or routine id to start instead of a prompt")
	cmd.Flags().StringVar(&workflowInput, "input", "", `workflow input as JSON, for example {"objective":"..."}`)
	cmd.Flags().StringVar(&overlap, "overlap", "skip", "overlap policy: skip or queue_one")
	cmd.Flags().DurationVar(&grace, "grace", 0, "catch-up grace (default from config)")
	return cmd
}

func newListCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List jobs with their next fire instant",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOpener(open); err != nil {
				return err
			}
			return open(cmd, false, func(ctx context.Context, env *CommandEnv) error {
				if env == nil || env.Store == nil || env.Service == nil {
					return fault.New(ErrorCodeInvalidArgument, "schedule command environment is incomplete")
				}
				jobs, err := env.Store.ListJobs(ctx, 200)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if _, err := fmt.Fprint(out, "ID\tNAME\tSCHEDULE\tTIMEZONE\tSTATE\tNEXT_FIRE\n"); err != nil {
					return err
				}
				now := time.Now().UTC()
				for i := range jobs {
					next := "-"
					if jobs[i].State == JobActive {
						if fire, err := env.Service.NextFire(ctx, jobs[i].ID, now); err == nil {
							next = fire.Format(time.RFC3339)
						}
					}
					if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\n",
						jobs[i].ID, jobs[i].Name, jobs[i].CronExpression,
						jobs[i].Timezone, jobs[i].State, next); err != nil {
						return err
					}
				}
				return ctx.Err()
			})
		},
	}
}

func newPauseCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "pause <id>",
		Short: "Pause a job after its in-flight turn settles",
		Args:  exactOneJobID,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOpener(open); err != nil {
				return err
			}
			return open(cmd, false, func(ctx context.Context, env *CommandEnv) error {
				if env == nil || env.Service == nil {
					return fault.New(ErrorCodeInvalidArgument, "schedule command environment is incomplete")
				}
				if err := env.Service.SetState(ctx, args[0], JobPaused); err != nil {
					return err
				}
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "id: %s\nstate: paused\n", args[0])
				return err
			})
		},
	}
}

func newResumeCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "resume <id>",
		Short: "Resume a paused job and restart its run",
		Args:  exactOneJobID,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOpener(open); err != nil {
				return err
			}
			return open(cmd, true, func(ctx context.Context, env *CommandEnv) error {
				if env == nil || env.Service == nil || env.Starter == nil {
					return fault.New(ErrorCodeInvalidArgument, "schedule command environment is incomplete")
				}
				if err := env.Service.SetState(ctx, args[0], JobActive); err != nil {
					return err
				}
				job, err := env.Service.Get(ctx, args[0])
				if err != nil {
					return err
				}
				if err := StartNext(ctx, &NextRequest{Starter: env.Starter, Job: &job, After: time.Now().UTC()}); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "id: %s\nstate: active\n", args[0])
				return err
			})
		},
	}
}

func newDeleteCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Soft-delete a job and retain its history",
		Args:  exactOneJobID,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOpener(open); err != nil {
				return err
			}
			return open(cmd, false, func(ctx context.Context, env *CommandEnv) error {
				if env == nil || env.Service == nil {
					return fault.New(ErrorCodeInvalidArgument, "schedule command environment is incomplete")
				}
				if err := env.Service.SetState(ctx, args[0], JobDeleted); err != nil {
					return err
				}
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "id: %s\nstate: deleted\n", args[0])
				return err
			})
		},
	}
}

func newRunsCmd(open Opener) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "runs <id>",
		Short: "List occurrence history for a job",
		Args:  exactOneJobID,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOpener(open); err != nil {
				return err
			}
			if limit <= 0 || limit > 1000 {
				return &cliUsageError{fmt.Errorf("%s: --limit must be between 1 and 1000", cmd.CommandPath())}
			}
			return open(cmd, false, func(ctx context.Context, env *CommandEnv) error {
				if env == nil || env.Store == nil {
					return fault.New(ErrorCodeInvalidArgument, "schedule command environment is incomplete")
				}
				items, err := env.Store.Occurrences(ctx, args[0], limit)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if _, err := fmt.Fprint(out, "ID\tSCHEDULED_FOR\tSTATE\tTURN\n"); err != nil {
					return err
				}
				for i := range items {
					if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\n",
						items[i].ID,
						items[i].ScheduledForUTC.Format(time.RFC3339),
						items[i].State,
						items[i].TurnID); err != nil {
						return err
					}
				}
				return ctx.Err()
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum rows to show")
	return cmd
}

func newRunNowCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "run-now <id>",
		Short: "Record a manual fire and start the job run",
		Args:  exactOneJobID,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOpener(open); err != nil {
				return err
			}
			return open(cmd, true, func(ctx context.Context, env *CommandEnv) error {
				if env == nil || env.Service == nil || env.Starter == nil {
					return fault.New(ErrorCodeInvalidArgument, "schedule command environment is incomplete")
				}
				occurrence, err := env.Service.RunNow(ctx, args[0], time.Now().UTC())
				if err != nil {
					return err
				}
				if err := StartFire(ctx, &FireRequest{Starter: env.Starter, JobID: occurrence.JobID, Instant: occurrence.ScheduledForUTC}); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "id: %s\noccurrence: %s\n", args[0], occurrence.ID)
				return err
			})
		},
	}
}
