package connector

const (
	maxQueryBytes     = 1024
	defaultResults    = 10
	maxResults        = 15
	maxRecipients     = 20
	maxAddressBytes   = 254
	maxLocalBytes     = 64
	maxSubjectBytes   = 998
	maxComposeBody    = 256 << 10
	maxReadBody       = 64 << 10
	maxReferences     = 10
	maxMessageIDBytes = 255
	maxHeaderRunes    = 1024
	maxListRunes      = 2048
	maxSnippetRunes   = 512
	maxAttachments    = 50
	maxMIMEDepth      = 8
	maxMIMEParts      = 128
	maxPartBytes      = 1 << 20
	maxTextBytes      = 4 * maxReadBody
	maxHTMLDepth      = 256
	maxFilenameRunes  = 255

	idPattern = `^[A-Za-z0-9_-]{1,64}$`

	foldWidth = 78
)
