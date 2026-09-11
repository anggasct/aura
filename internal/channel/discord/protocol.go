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
	eventReady         = "READY"
	eventMessageCreate = "MESSAGE_CREATE"
	eventGuildCreate   = "GUILD_CREATE"
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
