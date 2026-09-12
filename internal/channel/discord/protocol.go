package discord

import "encoding/json"

const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatACK   = 11
)

const (
	intentGuilds         = 1 << 0
	intentGuildMessages  = 1 << 9
	intentDirectMessages = 1 << 12
	intentMessageContent = 1 << 15
)

const requiredIntents = intentGuilds | intentGuildMessages | intentDirectMessages | intentMessageContent

const (
	eventReady             = "READY"
	eventMessageCreate     = "MESSAGE_CREATE"
	eventInteractionCreate = "INTERACTION_CREATE"
	eventGuildCreate       = "GUILD_CREATE"
)

const gatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"

type gatewayEnvelope struct {
	Op   int             `json:"op"`
	Data json.RawMessage `json:"d"`
	Seq  *int64          `json:"s"`
	Type string          `json:"t"`
}

type helloPayload struct {
	HeartbeatIntervalMs int64 `json:"heartbeat_interval"`
}

type readyPayload struct {
	SessionID        string      `json:"session_id"`
	ResumeGatewayURL string      `json:"resume_gateway_url"`
	User             userPayload `json:"user"`
}

type userPayload struct {
	ID  string `json:"id"`
	Bot bool   `json:"bot"`
}

type messagePayload struct {
	ID          string              `json:"id"`
	ChannelID   string              `json:"channel_id"`
	GuildID     string              `json:"guild_id"`
	Author      userPayload         `json:"author"`
	Content     string              `json:"content"`
	Mentions    []userPayload       `json:"mentions"`
	Attachments []attachmentPayload `json:"attachments"`
}

type identifyPayload struct {
	Token      string             `json:"token"`
	Intents    int                `json:"intents"`
	Properties identifyProperties `json:"properties"`
}

type identifyProperties struct {
	OS      string `json:"os"`
	Browser string `json:"browser"`
	Device  string `json:"device"`
}

type resumePayload struct {
	Token     string `json:"token"`
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
}

type sendEnvelope struct {
	Op   int `json:"op"`
	Data any `json:"d"`
}

const (
	interactionMessageComponent = 3
)

const (
	componentActionRow = 1
	componentButton    = 2
)

const (
	buttonStylePrimary = 1
	buttonStyleDanger  = 4
)

const (
	callbackDeferredUpdate = 6
	callbackUpdateMessage  = 7
)

type interactionPayload struct {
	ID        string                 `json:"id"`
	Token     string                 `json:"token"`
	Type      int                    `json:"type"`
	GuildID   string                 `json:"guild_id"`
	ChannelID string                 `json:"channel_id"`
	Member    *interactionMember     `json:"member"`
	User      *userPayload           `json:"user"`
	Message   *interactionMessageRef `json:"message"`
	Data      interactionData        `json:"data"`
}

type interactionMember struct {
	User userPayload `json:"user"`
}

type interactionMessageRef struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
}

type interactionData struct {
	CustomID      string `json:"custom_id"`
	ComponentType int    `json:"component_type"`
}

func (p *interactionPayload) actorID() string {
	if p.Member != nil && p.Member.User.ID != "" {
		return p.Member.User.ID
	}
	if p.User != nil {
		return p.User.ID
	}
	return ""
}

type componentPayload struct {
	Type       int             `json:"type"`
	Components []buttonPayload `json:"components,omitempty"`
}

type buttonPayload struct {
	Type     int    `json:"type"`
	Style    int    `json:"style"`
	Label    string `json:"label"`
	CustomID string `json:"custom_id"`
}
