package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/store"
)

func newScheduleCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cron",
		Short: "Manage scheduled recurring jobs",
	}
	cmd.AddCommand(
		newScheduleAddCmd(gf),
		newScheduleListCmd(gf),
		newSchedulePauseCmd(gf),
		newScheduleResumeCmd(gf),
		newScheduleDeleteCmd(gf),
		newScheduleRunsCmd(gf),
		newScheduleRunNowCmd(gf),
	)
	return cmd
}

func scheduleService(db *sql.DB) (*schedule.Service, error) {
	return schedule.NewService(&scheduleStore{store: store.NewScheduleStore(db)})
}

func newScheduleAddCmd(gf *globalFlags) *cobra.Command {
	var name, cronSchedule, timezone, channel, destination, prompt, overlap, workflowID, workflowInput string
	var grace time.Duration
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Create a scheduled job and start its run",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, db *sql.DB) error {
				if timezone == "" {
					timezone = cfg.Scheduler.DefaultTimezone
				}
				graceSeconds := int64(grace.Seconds())
				if !cmd.Flags().Changed("grace") {
					graceSeconds = int64(time.Duration(cfg.Scheduler.DefaultCatchUpGrace).Seconds())
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
						return &usageError{fmt.Errorf("%s requires --%s", cmd.CommandPath(), flag.name)}
					}
				}
				if (prompt == "") == (workflowID == "") {
					return &usageError{fmt.Errorf("%s requires exactly one of --prompt or --workflow", cmd.CommandPath())}
				}
				if workflowInput != "" && workflowID == "" {
					return &usageError{fmt.Errorf("%s --input requires --workflow", cmd.CommandPath())}
				}
				if workflowID != "" {
					if err := checkScheduledWorkflow(ctx, cfg, db, workflowID, workflowInput); err != nil {
						return err
					}
				}
				service, err := scheduleService(db)
				if err != nil {
					return err
				}
				job, err := service.Create(ctx, &schedule.JobSpec{
					Name: name, CronExpression: cronSchedule, Timezone: timezone,
					Prompt: prompt, OriginChannel: channel, OriginDestination: destination,
					OverlapPolicy: overlap, CatchUpGraceSeconds: graceSeconds,
					WorkflowID: workflowID, WorkflowInput: workflowInput,
				})
				if err != nil {
					return err
				}
				if err := startScheduleRunIfDurable(ctx, cmd.OutOrStdout(), cfg, logger, job.ID); err != nil {
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

func startScheduleRun(ctx context.Context, runtime durable.Runtime, jobID string) error {
	if runtime == nil {
		return errors.New("durable runtime must not be nil")
	}
	_, err := runtime.Start(ctx, durable.StartRequest{
		Handler: schedule.HandlerName, Key: jobID, Payload: []byte(jobID),
	})
	return err
}

func startScheduleRunIfDurable(ctx context.Context, out io.Writer, cfg *config.Config, logger *slog.Logger, jobID string) error {
	if cfg.Durable == nil || !cfg.Durable.Enabled {
		_, err := fmt.Fprintln(out, "note: the job run starts with the server")
		return err
	}
	runtime, err := durableRuntimeForConfig(cfg, logger)
	if err != nil {
		return err
	}
	return startScheduleRun(ctx, runtime, jobID)
}

func newScheduleListCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List jobs with their next fire instant",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, _ *config.Config, db *sql.DB) error {
				service, err := scheduleService(db)
				if err != nil {
					return err
				}
				jobs, err := store.NewScheduleStore(db).ListJobs(ctx, 200)
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
					if jobs[i].State == store.ScheduleJobActive {
						if fire, err := service.NextFire(ctx, jobs[i].ID, now); err == nil {
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
	return cmd
}

func newSchedulePauseCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pause <id>",
		Short: "Pause a job after its in-flight turn settles",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, _ *config.Config, db *sql.DB) error {
				service, err := scheduleService(db)
				if err != nil {
					return err
				}
				if err := service.SetState(ctx, args[0], schedule.JobPaused); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "id: %s\nstate: paused\n", args[0])
				return err
			})
		},
	}
	return cmd
}

func newScheduleResumeCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume <id>",
		Short: "Resume a paused job and restart its run",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, db *sql.DB) error {
				service, err := scheduleService(db)
				if err != nil {
					return err
				}
				if err := service.SetState(ctx, args[0], schedule.JobActive); err != nil {
					return err
				}
				if err := startScheduleRunIfDurable(ctx, cmd.OutOrStdout(), cfg, logger, args[0]); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "id: %s\nstate: active\n", args[0])
				return err
			})
		},
	}
	return cmd
}

func newScheduleDeleteCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Soft-delete a job and retain its history",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, _ *config.Config, db *sql.DB) error {
				service, err := scheduleService(db)
				if err != nil {
					return err
				}
				if err := service.SetState(ctx, args[0], schedule.JobDeleted); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "id: %s\nstate: deleted\n", args[0])
				return err
			})
		},
	}
	return cmd
}

func newScheduleRunsCmd(gf *globalFlags) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "runs <id>",
		Short: "List occurrence history for a job",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit <= 0 || limit > 1000 {
				return &usageError{fmt.Errorf("%s: --limit must be between 1 and 1000", cmd.CommandPath())}
			}
			return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, _ *config.Config, db *sql.DB) error {
				items, err := store.NewScheduleStore(db).Occurrences(ctx, args[0], limit)
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

func newScheduleRunNowCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run-now <id>",
		Short: "Record a manual fire and start the job run",
		Args:  exactOneArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStorage(cmd, gf, true, func(ctx context.Context, logger *slog.Logger, cfg *config.Config, db *sql.DB) error {
				service, err := scheduleService(db)
				if err != nil {
					return err
				}
				occurrence, err := service.RunNow(ctx, args[0], time.Now().UTC())
				if err != nil {
					return err
				}
				if err := startScheduleRunIfDurable(ctx, cmd.OutOrStdout(), cfg, logger, args[0]); err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "id: %s\noccurrence: %s\n", args[0], occurrence.ID)
				return err
			})
		},
	}
	return cmd
}

func checkScheduledWorkflow(ctx context.Context, cfg *config.Config, db *sql.DB, id, input string) error {
	if input != "" {
		decoder := json.NewDecoder(strings.NewReader(input))
		decoder.DisallowUnknownFields()
		var parsed struct {
			Objective string `json:"objective"`
		}
		if err := decoder.Decode(&parsed); err != nil {
			return &usageError{fmt.Errorf("--input must be a JSON object with an optional objective: %w", err)}
		}
	}
	specs, err := workflow.LoadDefinitionsDir(workflowDefinitionsDir(cfg))
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if spec.ID == id {
			return nil
		}
	}
	if _, err := workflow.NewStore(db).LatestDefinition(ctx, id); err != nil {
		return &usageError{fmt.Errorf("workflow %q is neither a definition nor a saved routine", id)}
	}
	return nil
}
