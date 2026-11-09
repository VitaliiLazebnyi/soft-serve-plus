package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/soft-serve/pkg/access"
	"github.com/charmbracelet/soft-serve/pkg/backend"
	"github.com/charmbracelet/soft-serve/pkg/utils"
	"github.com/charmbracelet/soft-serve/pkg/webhook"
	"github.com/dustin/go-humanize"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

func webhookCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "webhook",
		Aliases: []string{"webhooks"},
		Short:   "Manage repository webhooks",
	}

	cmd.AddCommand(
		webhookListCommand(),
		webhookCreateCommand(),
		webhookDeleteCommand(),
		webhookUpdateCommand(),
		webhookDeliveriesCommand(),
	)

	return cmd
}

var webhookEvents []string

func init() {
	events := webhook.Events()
	webhookEvents = make([]string, len(events))
	for i, e := range events {
		webhookEvents[i] = e.String()
	}
}

func webhookListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "list REPOSITORY",
		Short:             "List repository webhooks",
		Args:              cobra.ExactArgs(1),
		PersistentPreRunE: requireRepo(access.AdminAccess),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			repo, _, err := repoFromContext(ctx)
			if err != nil {
				return err
			}

			webhooks, err := be.ListWebhooks(ctx, repo)
			if err != nil {
				return err
			}

			table := table.New().Headers("ID", "URL", "Events", "Active", "Created At", "Updated At")
			for _, h := range webhooks {
				events := make([]string, len(h.Events))
				for i, e := range h.Events {
					events[i] = e.String()
				}

				table = table.Row(
					strconv.FormatInt(h.ID, 10),
					utils.Sanitize(h.URL),
					strings.Join(events, ","),
					strconv.FormatBool(h.Active),
					humanize.Time(h.CreatedAt),
					humanize.Time(h.UpdatedAt),
				)
			}
			cmd.Println(table)
			return nil
		},
	}

	return cmd
}

func webhookCreateCommand() *cobra.Command {
	var events []string
	var secret string
	var active bool
	var contentType string
	cmd := &cobra.Command{
		Use:               "create REPOSITORY URL",
		Short:             "Create a repository webhook",
		Args:              cobra.ExactArgs(2),
		PersistentPreRunE: requireRepo(access.AdminAccess),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			repo, _, err := repoFromContext(ctx)
			if err != nil {
				return err
			}

			evs, err := parseWebhookEvents(events)
			if err != nil {
				return err
			}

			ct, err := parseWebhookContentType(contentType)
			if err != nil {
				return err
			}

			url := utils.Sanitize(args[1])
			return be.CreateWebhook(ctx, repo, strings.TrimSpace(url), ct, secret, evs, active)
		},
	}

	cmd.Flags().StringSliceVarP(&events, "events", "e", nil, fmt.Sprintf("events to trigger the webhook, available events are (%s)", strings.Join(webhookEvents, ", ")))
	cmd.Flags().StringVarP(&secret, "secret", "s", "", "secret to sign the webhook payload")
	cmd.Flags().BoolVarP(&active, "active", "a", true, "whether the webhook is active")
	cmd.Flags().StringVarP(&contentType, "content-type", "c", "json", "content type of the webhook payload, can be either `json` or `form`")

	return cmd
}

// parseWebhookEvents parses the --events flag values of create and update.
func parseWebhookEvents(events []string) ([]webhook.Event, error) {
	evs := make([]webhook.Event, 0, len(events))
	for _, e := range events {
		ev, err := webhook.ParseEvent(e)
		if err != nil {
			return nil, fmt.Errorf("invalid event: %w", err)
		}

		evs = append(evs, ev)
	}

	return evs, nil
}

// parseWebhookContentType parses the --content-type flag value of create and
// update: "json" or "form", ignoring case and surrounding spaces.
func parseWebhookContentType(s string) (webhook.ContentType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "json":
		return webhook.ContentTypeJSON, nil
	case "form":
		return webhook.ContentTypeForm, nil
	default:
		return -1, webhook.ErrInvalidContentType
	}
}

func webhookDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "delete REPOSITORY WEBHOOK_ID",
		Short:             "Delete a repository webhook",
		Args:              cobra.ExactArgs(2),
		PersistentPreRunE: requireRepo(access.AdminAccess),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			repo, _, err := repoFromContext(ctx)
			if err != nil {
				return err
			}

			id, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid webhook ID: %w", err)
			}

			return be.DeleteWebhook(ctx, repo, id)
		},
	}

	return cmd
}

func webhookUpdateCommand() *cobra.Command {
	var events []string
	var secret string
	var active string
	var contentType string
	var url string
	cmd := &cobra.Command{
		Use:               "update REPOSITORY WEBHOOK_ID",
		Short:             "Update a repository webhook",
		Args:              cobra.ExactArgs(2),
		PersistentPreRunE: requireRepo(access.AdminAccess),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			repo, _, err := repoFromContext(ctx)
			if err != nil {
				return err
			}

			id, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid webhook ID: %w", err)
			}

			wh, err := be.Webhook(ctx, repo, id)
			if err != nil {
				return err
			}

			// A flag given on the command line replaces the stored value, even
			// when it is empty: --secret= removes the secret and --events=
			// unsubscribes from every event. Flags left out keep their value.
			flags := cmd.Flags()

			newURL := wh.URL
			if flags.Changed("url") {
				// Normalise the same way create does.
				newURL = strings.TrimSpace(utils.Sanitize(url))
			}

			newSecret := wh.Secret
			if flags.Changed("secret") {
				newSecret = secret
			}

			newActive := wh.Active
			if flags.Changed("active") {
				active, err := strconv.ParseBool(active)
				if err != nil {
					return fmt.Errorf("invalid active value: %w", err)
				}

				newActive = active
			}

			newContentType := wh.ContentType
			if flags.Changed("content-type") {
				newContentType, err = parseWebhookContentType(contentType)
				if err != nil {
					return err
				}
			}

			newEvents := wh.Events
			if flags.Changed("events") {
				newEvents, err = parseWebhookEvents(events)
				if err != nil {
					return err
				}
			}

			return be.UpdateWebhook(ctx, repo, id, newURL, newContentType, newSecret, newEvents, newActive)
		},
	}

	cmd.Flags().StringSliceVarP(&events, "events", "e", nil, fmt.Sprintf("events to trigger the webhook, replacing the current ones; available events are (%s); pass --events= to remove them all", strings.Join(webhookEvents, ", ")))
	cmd.Flags().StringVarP(&secret, "secret", "s", "", "secret to sign the webhook payload; pass --secret= to remove it")
	cmd.Flags().StringVarP(&active, "active", "a", "", "whether the webhook is active")
	cmd.Flags().StringVarP(&contentType, "content-type", "c", "", "content type of the webhook payload, can be either `json` or `form`")
	cmd.Flags().StringVarP(&url, "url", "u", "", "webhook URL")

	return cmd
}

func webhookDeliveriesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "deliveries",
		Short:   "Manage webhook deliveries",
		Aliases: []string{"delivery", "deliver"},
	}

	cmd.AddCommand(
		webhookDeliveriesListCommand(),
		webhookDeliveriesRedeliverCommand(),
		webhookDeliveriesGetCommand(),
	)

	return cmd
}

func webhookDeliveriesListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "list REPOSITORY WEBHOOK_ID",
		Short:             "List webhook deliveries",
		Args:              cobra.ExactArgs(2),
		PersistentPreRunE: requireRepo(access.AdminAccess),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			repo, _, err := repoFromContext(ctx)
			if err != nil {
				return err
			}

			id, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid webhook ID: %w", err)
			}

			dels, err := be.ListWebhookDeliveries(ctx, repo, id)
			if err != nil {
				return err
			}

			table := table.New().Headers("Status", "ID", "Event", "Created At")
			for _, d := range dels {
				status := "❌"
				if d.ResponseStatus >= 200 && d.ResponseStatus < 300 {
					status = "✅"
				}
				table = table.Row(
					status,
					d.ID.String(),
					d.Event.String(),
					humanize.Time(d.CreatedAt),
				)
			}
			cmd.Println(table)
			return nil
		},
	}

	return cmd
}

func webhookDeliveriesRedeliverCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "redeliver REPOSITORY WEBHOOK_ID DELIVERY_ID",
		Short:             "Redeliver a webhook delivery",
		Args:              cobra.ExactArgs(3),
		PersistentPreRunE: requireRepo(access.AdminAccess),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			repo, _, err := repoFromContext(ctx)
			if err != nil {
				return err
			}

			id, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid webhook ID: %w", err)
			}

			delID, err := uuid.Parse(args[2])
			if err != nil {
				return fmt.Errorf("invalid delivery ID: %w", err)
			}

			return be.RedeliverWebhookDelivery(ctx, repo, id, delID)
		},
	}

	return cmd
}

func webhookDeliveriesGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "get REPOSITORY WEBHOOK_ID DELIVERY_ID",
		Short:             "Get a webhook delivery",
		Args:              cobra.ExactArgs(3),
		PersistentPreRunE: requireRepo(access.AdminAccess),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			be := backend.FromContext(ctx)
			repo, _, err := repoFromContext(ctx)
			if err != nil {
				return err
			}

			id, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid webhook ID: %w", err)
			}

			delID, err := uuid.Parse(args[2])
			if err != nil {
				return fmt.Errorf("invalid delivery ID: %w", err)
			}

			del, err := be.WebhookDelivery(ctx, repo, id, delID)
			if err != nil {
				return err
			}

			// The response headers and body come from the receiver, and the
			// request carries repository data, so none of it may reach the
			// admin's terminal as escape sequences.
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "ID: %s\n", del.ID)                                             //nolint:errcheck
			fmt.Fprintf(out, "Event: %s\n", del.Event)                                       //nolint:errcheck
			fmt.Fprintf(out, "Request URL: %s\n", utils.Sanitize(del.RequestURL))            //nolint:errcheck
			fmt.Fprintf(out, "Request Method: %s\n", utils.Sanitize(del.RequestMethod))      //nolint:errcheck
			fmt.Fprintf(out, "Request Error: %s\n", utils.Sanitize(del.RequestError.String)) //nolint:errcheck
			fmt.Fprintf(out, "Request Headers:\n")                                           //nolint:errcheck
			reqHeaders := strings.Split(utils.Sanitize(del.RequestHeaders), "\n")
			for _, h := range reqHeaders {
				fmt.Fprintf(out, "  %s\n", h) //nolint:errcheck
			}

			fmt.Fprintf(out, "Request Body:\n") //nolint:errcheck
			reqBody := strings.Split(utils.Sanitize(del.RequestBody), "\n")
			for _, b := range reqBody {
				fmt.Fprintf(out, "  %s\n", b) //nolint:errcheck
			}

			fmt.Fprintf(out, "Response Status: %d\n", del.ResponseStatus) //nolint:errcheck
			fmt.Fprintf(out, "Response Headers:\n")                       //nolint:errcheck
			resHeaders := strings.Split(utils.Sanitize(del.ResponseHeaders), "\n")
			for _, h := range resHeaders {
				fmt.Fprintf(out, "  %s\n", h) //nolint:errcheck
			}

			fmt.Fprintf(out, "Response Body:\n") //nolint:errcheck
			resBody := strings.Split(utils.Sanitize(del.ResponseBody), "\n")
			for _, b := range resBody {
				fmt.Fprintf(out, "  %s\n", b) //nolint:errcheck
			}

			return nil
		},
	}

	return cmd
}
