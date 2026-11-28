package agent

import (
	"maps"
	"regexp"
	"slices"
	"strings"

	policy "github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"golang.org/x/text/language"
)

const (
	DefaultMessageLanguage = "en"
	MessageLanguageID      = "id"
)

const (
	PersonaPlaceholder   = "{persona}"
	CountPlaceholder     = "{count}"
	CommandPlaceholder   = "{command}"
	SkillPlaceholder     = "{skill}"
	UsagePlaceholder     = "{usage}"
	ActionPlaceholder    = "{action}"
	ToolPlaceholder      = "{tool}"
	RequesterPlaceholder = "{requester}"
	RunPlaceholder       = "{run}"
	DurationPlaceholder  = "{duration}"
	TimePlaceholder      = "{time}"
	NumberPlaceholder    = "{n}"
)

const (
	MessagePairingCode                 = "pairing_code"
	MessagePairingPending              = "pairing_pending"
	MessagePairingLocked               = "pairing_locked"
	MessageWaitingForOwner             = "waiting_for_owner"
	MessageApprovalTimeout             = "approval_timeout"
	MessageApprovalDenied              = "approval_denied"
	MessageBudgetExhausted             = "budget_exhausted"
	MessageRateLimited                 = "rate_limited"
	MessageNotPermitted                = "not_permitted"
	MessageHandoffOpen                 = "handoff_open"
	MessageHandoffClosed               = "handoff_closed"
	MessageForgetmeConfirm             = "forgetme_confirm"
	MessageForgetmeDone                = "forgetme_done"
	MessageForgetmeStopping            = "forgetme_stopping"
	MessageUnavailable                 = "unavailable"
	MessageWorkflowSucceeded           = "workflow_succeeded"
	MessageWorkflowFailed              = "workflow_failed"
	MessageWorkflowCancelled           = "workflow_cancelled"
	MessageTurnFailed                  = "turn_failed"
	MessageTurnTimeout                 = "turn_timeout"
	MessageTurnEmpty                   = "turn_empty"
	MessageStatusWorking               = "status_working"
	MessageStatusStillWorking          = "status_still_working"
	MessageStatusStopped               = "status_stopped"
	MessageButtonStop                  = "button_stop"
	MessageCommandNew                  = "command_new"
	MessageCommandSessionCurrent       = "command_session_current"
	MessageCommandSessionSwitched      = "command_session_switched"
	MessageCommandStatusPersona        = "command_status_persona"
	MessageCommandIdle                 = "command_idle"
	MessageCommandStatusRunning        = "command_status_running"
	MessageCommandStatusRunningQueued  = "command_status_running_queued"
	MessageCommandCancelled            = "command_cancelled"
	MessageCommandDenied               = "command_denied"
	MessageCommandGuestsOnly           = "command_guests_only"
	MessageCommandUnavailable          = "command_unavailable"
	MessageCommandScope                = "command_scope"
	MessageSessionNotSwitchable        = "session_not_switchable"
	MessageSkillUnavailable            = "skill_unavailable"
	MessageCommandFailed               = "command_failed"
	MessageCommandUsage                = "command_usage"
	MessageCommandUnknown              = "command_unknown"
	MessageApprovalSummary             = "approval_summary"
	MessageApprovalActionExec          = "approval_action_exec"
	MessageApprovalActionCodeExecute   = "approval_action_code_execute"
	MessageApprovalActionWriteFile     = "approval_action_write_file"
	MessageApprovalActionGitHubCreate  = "approval_action_github_create_pr"
	MessageApprovalActionGitHubMerge   = "approval_action_github_merge"
	MessageApprovalActionGitHubComment = "approval_action_github_comment"
	MessageApprovalActionEmailSend     = "approval_action_email_send"
	MessageApprovalActionGeneric       = "approval_action_generic"
	MessageApprovalRequestedBy         = "approval_requested_by"
	MessageApprovalWorkflowRun         = "approval_workflow_run"
	MessageApprovalUntrustedInput      = "approval_untrusted_input"
	MessageButtonApprove               = "button_approve"
	MessageButtonReject                = "button_reject"
	MessageButtonAllowFor              = "button_allow_for"
	MessageDurationDays                = "duration_days"
	MessageDurationDay                 = "duration_day"
	MessageDurationHours               = "duration_hours"
	MessageCardApproved                = "card_approved"
	MessageCardAllowedFor              = "card_allowed_for"
	MessageCardRejected                = "card_rejected"
	MessageCardExpired                 = "card_expired"
	MessageCardCancelled               = "card_cancelled"
)

var (
	placeholderPattern  = regexp.MustCompile(`\{[A-Za-z0-9_]+\}`)
	supportedBundleTags = []language.Tag{language.English, language.Indonesian}
	supportedBundleKeys = []string{DefaultMessageLanguage, MessageLanguageID}
	bundleMatcher       = language.NewMatcher(supportedBundleTags)
)

var bundleOnlyMessages = map[string]bool{
	MessageApprovalActionExec:          true,
	MessageApprovalActionCodeExecute:   true,
	MessageApprovalActionWriteFile:     true,
	MessageApprovalActionGitHubCreate:  true,
	MessageApprovalActionGitHubMerge:   true,
	MessageApprovalActionGitHubComment: true,
	MessageApprovalActionEmailSend:     true,
	MessageApprovalUntrustedInput:      true,
}

var messagePlaceholders = map[string][]string{
	MessagePairingCode:                {PairingCodePlaceholder},
	MessageWorkflowSucceeded:          {WorkflowGoalPlaceholder},
	MessageWorkflowFailed:             {WorkflowGoalPlaceholder},
	MessageWorkflowCancelled:          {WorkflowGoalPlaceholder},
	MessageCommandStatusPersona:       {PersonaPlaceholder},
	MessageCommandStatusRunningQueued: {CountPlaceholder},
	MessageCommandUnavailable:         {CommandPlaceholder},
	MessageCommandScope:               {CommandPlaceholder},
	MessageSkillUnavailable:           {SkillPlaceholder},
	MessageCommandFailed:              {CommandPlaceholder},
	MessageCommandUsage:               {CommandPlaceholder, UsagePlaceholder},
	MessageApprovalSummary:            {PersonaPlaceholder, ActionPlaceholder},
	MessageApprovalActionGeneric:      {ToolPlaceholder},
	MessageApprovalRequestedBy:        {RequesterPlaceholder},
	MessageApprovalWorkflowRun:        {RunPlaceholder},
	MessageButtonAllowFor:             {DurationPlaceholder},
	MessageDurationDays:               {NumberPlaceholder},
	MessageDurationHours:              {NumberPlaceholder},
	MessageCardApproved:               {TimePlaceholder},
	MessageCardAllowedFor:             {DurationPlaceholder, TimePlaceholder},
	MessageCardRejected:               {TimePlaceholder},
	MessageCardExpired:                {TimePlaceholder},
	MessageCardCancelled:              {TimePlaceholder},
}

var messageBundles = map[string]map[string]string{
	DefaultMessageLanguage: {
		MessagePairingCode:                 "To talk with me, ask the owner to approve this pairing code: " + PairingCodePlaceholder,
		MessagePairingPending:              "A pairing request is already pending. Please wait for the owner.",
		MessagePairingLocked:               "Too many pairing attempts. Please try again later.",
		MessageWaitingForOwner:             "One moment, I am checking with the owner.",
		MessageApprovalTimeout:             "The owner did not respond in time, so the request was cancelled. Please try again later.",
		MessageApprovalDenied:              "The owner declined this request.",
		MessageBudgetExhausted:             "Your daily quota is used up. Please try again tomorrow.",
		MessageRateLimited:                 "You are sending messages too quickly. Please wait a moment.",
		MessageNotPermitted:                "That is not available here.",
		MessageHandoffOpen:                 "I have passed this conversation to the owner, who will reply here.",
		MessageHandoffClosed:               "The owner has handed the conversation back to me.",
		MessageForgetmeConfirm:             "This erases your conversations and data with me. Send /forgetme confirm to proceed.",
		MessageForgetmeDone:                "Your data has been erased.",
		MessageForgetmeStopping:            "Your workflow runs are being stopped. Please send /forgetme confirm again in a minute.",
		MessageUnavailable:                 "This assistant is not available right now.",
		MessageWorkflowSucceeded:           "Done: " + WorkflowGoalPlaceholder,
		MessageWorkflowFailed:              "Sorry, " + WorkflowGoalPlaceholder + " could not be completed.",
		MessageWorkflowCancelled:           WorkflowGoalPlaceholder + " was cancelled.",
		MessageTurnFailed:                  "Sorry, something went wrong and I could not finish this. Please try again.",
		MessageTurnTimeout:                 "Sorry, this took too long and I had to stop. Please try again, maybe with a smaller request.",
		MessageTurnEmpty:                   "Sorry, I could not put an answer together. Please try again or rephrase.",
		MessageStatusWorking:               "Working…",
		MessageStatusStillWorking:          "Still working…",
		MessageStatusStopped:               "⏹ Stopped",
		MessageButtonStop:                  "Stop",
		MessageCommandNew:                  "Started a new conversation.",
		MessageCommandSessionCurrent:       "This is the current conversation. Send /new to start a fresh one.",
		MessageCommandSessionSwitched:      "Switched to that conversation.",
		MessageCommandStatusPersona:        "Persona: " + PersonaPlaceholder,
		MessageCommandIdle:                 "Nothing is running.",
		MessageCommandStatusRunning:        "Working on a request.",
		MessageCommandStatusRunningQueued:  "Working on a request; " + CountPlaceholder + " more waiting.",
		MessageCommandCancelled:            "Stopped the current request.",
		MessageCommandDenied:               "That command is only available to the owner.",
		MessageCommandGuestsOnly:           "That command is not available to the owner.",
		MessageCommandUnavailable:          "/" + CommandPlaceholder + " only works in the terminal.",
		MessageCommandScope:                "Use /" + CommandPlaceholder + " inside an Aura thread or a DM.",
		MessageSessionNotSwitchable:        "That conversation can't be used here.",
		MessageSkillUnavailable:            "Skill " + SkillPlaceholder + " is not available.",
		MessageCommandFailed:               "/" + CommandPlaceholder + " failed; check aura status.",
		MessageCommandUsage:                "Usage: /" + CommandPlaceholder + " " + UsagePlaceholder,
		MessageCommandUnknown:              "Unknown command. Try /help.",
		MessageApprovalSummary:             PersonaPlaceholder + " wants to " + ActionPlaceholder + ":",
		MessageApprovalActionExec:          "run a command",
		MessageApprovalActionCodeExecute:   "run code",
		MessageApprovalActionWriteFile:     "write a file",
		MessageApprovalActionGitHubCreate:  "open a pull request",
		MessageApprovalActionGitHubMerge:   "merge a pull request",
		MessageApprovalActionGitHubComment: "post a comment on GitHub",
		MessageApprovalActionEmailSend:     "send an email",
		MessageApprovalActionGeneric:       "use " + ToolPlaceholder,
		MessageApprovalRequestedBy:         "Asked by " + RequesterPlaceholder,
		MessageApprovalWorkflowRun:         "Workflow run " + RunPlaceholder,
		MessageApprovalUntrustedInput:      policy.UntrustedInputNotice,
		MessageButtonApprove:               "Approve",
		MessageButtonReject:                "Reject",
		MessageButtonAllowFor:              "Allow for " + DurationPlaceholder,
		MessageDurationDays:                NumberPlaceholder + " days",
		MessageDurationDay:                 "1 day",
		MessageDurationHours:               NumberPlaceholder + " hours",
		MessageCardApproved:                "✅ Approved · " + TimePlaceholder,
		MessageCardAllowedFor:              "✅ Allowed for " + DurationPlaceholder + " · " + TimePlaceholder,
		MessageCardRejected:                "❌ Rejected · " + TimePlaceholder,
		MessageCardExpired:                 "⌛ Expired · " + TimePlaceholder,
		MessageCardCancelled:               "⏹ Cancelled · " + TimePlaceholder,
	},
	MessageLanguageID: {
		MessagePairingCode:                 "Untuk ngobrol denganku, minta pemilik menyetujui kode pairing ini: " + PairingCodePlaceholder,
		MessagePairingPending:              "Permintaan pairing sudah menunggu. Tunggu pemilik, ya.",
		MessagePairingLocked:               "Terlalu banyak percobaan pairing. Coba lagi nanti.",
		MessageWaitingForOwner:             "Sebentar, aku tanya pemilik dulu.",
		MessageApprovalTimeout:             "Pemilik belum menjawab, jadi permintaan ini dibatalkan. Coba lagi nanti.",
		MessageApprovalDenied:              "Pemilik menolak permintaan ini.",
		MessageBudgetExhausted:             "Kuota harianmu sudah habis. Coba lagi besok.",
		MessageRateLimited:                 "Pesannya terlalu cepat. Tunggu sebentar, ya.",
		MessageNotPermitted:                "Itu tidak tersedia di sini.",
		MessageHandoffOpen:                 "Percakapan ini sudah kuteruskan ke pemilik, nanti pemilik yang membalas di sini.",
		MessageHandoffClosed:               "Pemilik sudah menyerahkan percakapan ini kembali kepadaku.",
		MessageForgetmeConfirm:             "Ini akan menghapus percakapan dan datamu bersamaku. Kirim /forgetme confirm untuk lanjut.",
		MessageForgetmeDone:                "Datamu sudah dihapus.",
		MessageForgetmeStopping:            "Workflow-mu sedang dihentikan. Kirim /forgetme confirm lagi sebentar lagi.",
		MessageUnavailable:                 "Asisten ini sedang tidak tersedia.",
		MessageWorkflowSucceeded:           "Selesai: " + WorkflowGoalPlaceholder,
		MessageWorkflowFailed:              "Maaf, " + WorkflowGoalPlaceholder + " tidak bisa diselesaikan.",
		MessageWorkflowCancelled:           WorkflowGoalPlaceholder + " dibatalkan.",
		MessageTurnFailed:                  "Maaf, ada yang salah dan aku tidak bisa menyelesaikannya. Coba lagi, ya.",
		MessageTurnTimeout:                 "Maaf, ini terlalu lama jadi aku hentikan. Coba lagi, mungkin dengan permintaan yang lebih kecil.",
		MessageTurnEmpty:                   "Maaf, aku belum bisa menyusun jawabannya. Coba lagi atau ulangi dengan kata lain.",
		MessageStatusWorking:               "Sedang mengerjakan…",
		MessageStatusStillWorking:          "Masih mengerjakan…",
		MessageStatusStopped:               "⏹ Dihentikan",
		MessageButtonStop:                  "Hentikan",
		MessageCommandNew:                  "Percakapan baru dimulai.",
		MessageCommandSessionCurrent:       "Ini percakapan yang sedang aktif. Kirim /new untuk mulai yang baru.",
		MessageCommandSessionSwitched:      "Sudah pindah ke percakapan itu.",
		MessageCommandStatusPersona:        "Persona: " + PersonaPlaceholder,
		MessageCommandIdle:                 "Tidak ada yang sedang berjalan.",
		MessageCommandStatusRunning:        "Sedang mengerjakan permintaan.",
		MessageCommandStatusRunningQueued:  "Sedang mengerjakan permintaan; " + CountPlaceholder + " lagi menunggu.",
		MessageCommandCancelled:            "Permintaan yang berjalan dihentikan.",
		MessageCommandDenied:               "Perintah itu hanya untuk pemilik.",
		MessageCommandGuestsOnly:           "Perintah itu tidak tersedia untuk pemilik.",
		MessageCommandUnavailable:          "/" + CommandPlaceholder + " hanya bisa dipakai di terminal.",
		MessageCommandScope:                "Pakai /" + CommandPlaceholder + " di dalam thread Aura atau DM.",
		MessageSessionNotSwitchable:        "Percakapan itu tidak bisa dipakai di sini.",
		MessageSkillUnavailable:            "Skill " + SkillPlaceholder + " tidak tersedia.",
		MessageCommandFailed:               "/" + CommandPlaceholder + " gagal; cek aura status.",
		MessageCommandUsage:                "Cara pakai: /" + CommandPlaceholder + " " + UsagePlaceholder,
		MessageCommandUnknown:              "Perintah tidak dikenal. Coba /help.",
		MessageApprovalSummary:             PersonaPlaceholder + " mau " + ActionPlaceholder + ":",
		MessageApprovalActionExec:          "menjalankan perintah",
		MessageApprovalActionCodeExecute:   "menjalankan kode",
		MessageApprovalActionWriteFile:     "menulis file",
		MessageApprovalActionGitHubCreate:  "membuat pull request",
		MessageApprovalActionGitHubMerge:   "menggabungkan pull request",
		MessageApprovalActionGitHubComment: "mengirim komentar di GitHub",
		MessageApprovalActionEmailSend:     "mengirim email",
		MessageApprovalActionGeneric:       "memakai " + ToolPlaceholder,
		MessageApprovalRequestedBy:         "Diminta oleh " + RequesterPlaceholder,
		MessageApprovalWorkflowRun:         "Workflow run " + RunPlaceholder,
		MessageApprovalUntrustedInput:      "input tidak tepercaya: isi dari luar kendalimu membentuk permintaan ini, jadi hanya bisa disetujui sekali",
		MessageButtonApprove:               "Izinkan",
		MessageButtonReject:                "Tolak",
		MessageButtonAllowFor:              "Izinkan " + DurationPlaceholder,
		MessageDurationDays:                NumberPlaceholder + " hari",
		MessageDurationDay:                 "1 hari",
		MessageDurationHours:               NumberPlaceholder + " jam",
		MessageCardApproved:                "✅ Diizinkan · " + TimePlaceholder,
		MessageCardAllowedFor:              "✅ Diizinkan " + DurationPlaceholder + " · " + TimePlaceholder,
		MessageCardRejected:                "❌ Ditolak · " + TimePlaceholder,
		MessageCardExpired:                 "⌛ Kedaluwarsa · " + TimePlaceholder,
		MessageCardCancelled:               "⏹ Dibatalkan · " + TimePlaceholder,
	},
}

func DefaultMessage(key string) string {
	return messageBundles[DefaultMessageLanguage][key]
}

func IsBundleOnlyMessage(key string) bool {
	return bundleOnlyMessages[key]
}

func PersonaMessageDefaults() map[string]string {
	return maps.Clone(messageBundles[DefaultMessageLanguage])
}

func MessagePlaceholders(key string) []string {
	return slices.Clone(messagePlaceholders[key])
}

func selectMessageBundle(raw string) (bundle map[string]string, fallback string) {
	if raw == "" {
		return messageBundles[DefaultMessageLanguage], ""
	}
	tag, err := language.Parse(raw)
	if err != nil {
		return messageBundles[DefaultMessageLanguage], raw
	}
	_, idx, conf := bundleMatcher.Match(tag)
	if (conf == language.Exact || conf == language.High) && sameBaseLanguage(tag, supportedBundleTags[idx]) {
		return messageBundles[supportedBundleKeys[idx]], ""
	}
	return messageBundles[DefaultMessageLanguage], tag.String()
}

func sameBaseLanguage(tag, candidate language.Tag) bool {
	base, conf := tag.Base()
	if conf != language.Exact {
		return false
	}
	wantBase, _ := candidate.Base()
	return base == wantBase
}

func validateMessagePlaceholders(key, value string) error {
	required := messagePlaceholders[key]
	for _, placeholder := range required {
		if !strings.Contains(value, placeholder) {
			return fault.New(ErrorCodePersonaInvalid, "must contain "+placeholder)
		}
	}
	for _, found := range placeholderPattern.FindAllString(value, -1) {
		if !slices.Contains(required, found) {
			return fault.New(ErrorCodePersonaInvalid, "must not contain "+found)
		}
	}
	return nil
}
