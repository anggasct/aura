package config

import (
	"strings"
	"testing"
)

func embeddingConfig(fields string) string {
	return "version: 1\nknowledge:\n  embedding:\n" + fields
}

const validEmbeddingFields = "    base_url: https://api.openai.com/v1\n    model: text-embedding-3-small\n    dimensions: 1536\n"

func TestLoad_KnowledgeEmbeddingIsAbsentByDefault(t *testing.T) {
	result, err := Load(writeDiscordConfig(t, "version: 1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if result.Config.Knowledge != nil {
		t.Fatalf("Knowledge = %+v, want nil", result.Config.Knowledge)
	}
}

func TestLoad_KnowledgeEmbeddingAppliesDefaults(t *testing.T) {
	result, err := Load(writeDiscordConfig(t, embeddingConfig(validEmbeddingFields)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	embedding := result.Config.Knowledge.Embedding
	if embedding.DimensionsField != "dimensions" || embedding.RoleStyle != EmbeddingRoleNone ||
		embedding.BatchSize != 96 || embedding.MaxMemory != 256<<20 || embedding.PublicQueries || embedding.CredentialRef != "" {
		t.Fatalf("defaults = %+v", embedding)
	}
	endpoint, err := embedding.Endpoint()
	if err != nil || endpoint.Loopback || endpoint.Host != "api.openai.com" || endpoint.Port != "443" || endpoint.Path != "/v1" {
		t.Fatalf("Endpoint = %+v, %v", endpoint, err)
	}
}

func TestLoad_KnowledgeEmbeddingReadsEveryField(t *testing.T) {
	fields := `    base_url: https://api.voyageai.com/v1/
    model: voyage-3.5
    dimensions: 1024
    dimensions_field: output_dimension
    credential_ref: env://VOYAGE_API_KEY
    role_style: input_type
    batch_size: 128
    max_memory: 512MiB
    public_queries: true
`
	result, err := Load(writeDiscordConfig(t, embeddingConfig(fields)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	embedding := result.Config.Knowledge.Embedding
	if embedding.DimensionsField != "output_dimension" || embedding.RoleStyle != EmbeddingRoleInputType ||
		embedding.BatchSize != 128 || embedding.MaxMemory != 512<<20 || !embedding.PublicQueries ||
		embedding.CredentialRef != "env://VOYAGE_API_KEY" {
		t.Fatalf("embedding = %+v", embedding)
	}
	endpoint, err := embedding.Endpoint()
	if err != nil || endpoint.Path != "/v1" {
		t.Fatalf("Endpoint = %+v, %v; want the trailing slash trimmed", endpoint, err)
	}
}

func TestLoad_KnowledgeEmbeddingPrefixStyle(t *testing.T) {
	fields := validEmbeddingFields + "    role_style: prefix\n    query_prefix: \"task: search result | query: \"\n    document_prefix: \"title: none | text: \"\n"
	result, err := Load(writeDiscordConfig(t, embeddingConfig(fields)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	embedding := result.Config.Knowledge.Embedding
	if embedding.QueryPrefix != "task: search result | query: " || embedding.DocumentPrefix != "title: none | text: " {
		t.Fatalf("prefixes = %q %q", embedding.QueryPrefix, embedding.DocumentPrefix)
	}
}

func TestLoad_KnowledgeEmbeddingAcceptsLiteralLoopbackOnly(t *testing.T) {
	tests := map[string]struct {
		baseURL string
		port    string
	}{
		"ipv4 http":  {baseURL: "http://127.0.0.1:11434/v1", port: "11434"},
		"ipv4 https": {baseURL: "https://127.0.0.1/v1", port: "443"},
		"ipv6 http":  {baseURL: "http://[::1]:11434/v1", port: "11434"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fields := "    base_url: " + test.baseURL + "\n    model: nomic-embed-text\n    dimensions: 768\n"
			result, err := Load(writeDiscordConfig(t, embeddingConfig(fields)))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			endpoint, err := result.Config.Knowledge.Embedding.Endpoint()
			if err != nil || !endpoint.Loopback || endpoint.Port != test.port {
				t.Fatalf("Endpoint = %+v, %v", endpoint, err)
			}
		})
	}
}

func TestLoad_KnowledgeEmbeddingRejectsLoopbackLookalikes(t *testing.T) {
	for _, baseURL := range []string{
		"http://localhost:11434/v1",
		"https://localhost/v1",
		"http://LOCALHOST/v1",
		"http://localhost./v1",
		"http://embed.localhost/v1",
		"http://127.0.0.2/v1",
		"http://127.1/v1",
		"http://[::ffff:127.0.0.1]/v1",
		"http://127.0.0.1./v1",
		"http://[0:0:0:0:0:0:0:1]/v1",
		"http://2130706433/v1",
		"http://0x7f.0.0.1/v1",
		"https://127.0.0.2/v1",
		"https://[::ffff:7f00:1]/v1",
		"http://[::1%25lo]/v1",
	} {
		t.Run(baseURL, func(t *testing.T) {
			fields := "    base_url: \"" + baseURL + "\"\n    model: nomic-embed-text\n    dimensions: 768\n"
			if _, err := Load(writeDiscordConfig(t, embeddingConfig(fields))); err == nil {
				t.Fatalf("Load accepted %s", baseURL)
			}
		})
	}
}

func TestLoad_KnowledgeEmbeddingRejectsInvalidSettings(t *testing.T) {
	tests := map[string]struct {
		fields string
		detail string
	}{
		"missing base url":          {fields: "    model: m\n    dimensions: 8\n", detail: "base_url must be set"},
		"plain http remote":         {fields: "    base_url: http://api.example.com/v1\n    model: m\n    dimensions: 8\n", detail: "must use https"},
		"embedded credentials":      {fields: "    base_url: https://user:pw@api.example.com/v1\n    model: m\n    dimensions: 8\n", detail: "must not embed credentials"},
		"query string":              {fields: "    base_url: https://api.example.com/v1?key=1\n    model: m\n    dimensions: 8\n", detail: "query or fragment"},
		"fragment":                  {fields: "    base_url: https://api.example.com/v1#x\n    model: m\n    dimensions: 8\n", detail: "query or fragment"},
		"ftp scheme":                {fields: "    base_url: ftp://api.example.com/v1\n    model: m\n    dimensions: 8\n", detail: "scheme must be http or https"},
		"private literal":           {fields: "    base_url: https://10.0.0.5/v1\n    model: m\n    dimensions: 8\n", detail: "not a permitted destination"},
		"metadata literal":          {fields: "    base_url: https://169.254.169.254/v1\n    model: m\n    dimensions: 8\n", detail: "not a permitted destination"},
		"bad port":                  {fields: "    base_url: https://api.example.com:0/v1\n    model: m\n    dimensions: 8\n", detail: "port must be between"},
		"dot segment":               {fields: "    base_url: https://api.example.com/a/../v1\n    model: m\n    dimensions: 8\n", detail: "path must be plain"},
		"encoded path":              {fields: "    base_url: https://api.example.com/a%2Fb\n    model: m\n    dimensions: 8\n", detail: "path must be plain"},
		"missing model":             {fields: "    base_url: https://api.example.com/v1\n    dimensions: 8\n", detail: "model is required"},
		"model with space":          {fields: "    base_url: https://api.example.com/v1\n    model: \"a b\"\n    dimensions: 8\n", detail: "model is required"},
		"missing dimensions":        {fields: "    base_url: https://api.example.com/v1\n    model: m\n", detail: "dimensions is required"},
		"zero dimensions":           {fields: "    base_url: https://api.example.com/v1\n    model: m\n    dimensions: 0\n", detail: "dimensions is required"},
		"huge dimensions":           {fields: "    base_url: https://api.example.com/v1\n    model: m\n    dimensions: 100000\n", detail: "dimensions is required"},
		"reserved dimensions field": {fields: validEmbeddingFields + "    dimensions_field: model\n", detail: "dimensions_field must not be"},
		"bad dimensions field":      {fields: validEmbeddingFields + "    dimensions_field: Dim-Size\n", detail: "dimensions_field must be"},
		"bad credential ref":        {fields: validEmbeddingFields + "    credential_ref: OPENAI_KEY\n", detail: "credential_ref must use"},
		"empty env credential":      {fields: validEmbeddingFields + "    credential_ref: env://\n", detail: "credential_ref must use"},
		"unknown role style":        {fields: validEmbeddingFields + "    role_style: task\n", detail: "role_style must be"},
		"prefix without style":      {fields: validEmbeddingFields + "    query_prefix: \"q: \"\n", detail: "require role_style prefix"},
		"prefix style without text": {fields: validEmbeddingFields + "    role_style: prefix\n", detail: "requires query_prefix or document_prefix"},
		"control in prefix":         {fields: validEmbeddingFields + "    role_style: prefix\n    query_prefix: \"a\\nb\"\n", detail: "query_prefix must be"},
		"batch zero":                {fields: validEmbeddingFields + "    batch_size: 0\n", detail: "batch_size must be between"},
		"batch above limit":         {fields: validEmbeddingFields + "    batch_size: 1001\n", detail: "batch_size must be between"},
		"tiny memory":               {fields: validEmbeddingFields + "    max_memory: 1KiB\n", detail: "max_memory must be at least"},
		"unknown key":               {fields: validEmbeddingFields + "    temperature: 1\n", detail: "unknown key"},
		"string dimensions":         {fields: "    base_url: https://api.example.com/v1\n    model: m\n    dimensions: \"8\"\n", detail: "dimensions must be an integer"},
		"string public queries":     {fields: validEmbeddingFields + "    public_queries: \"yes\"\n", detail: "public_queries must be a boolean"},
		"embedding not mapping":     {fields: "    - a\n", detail: "must be a mapping"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeDiscordConfig(t, embeddingConfig(test.fields)))
			if err == nil || !strings.Contains(err.Error(), test.detail) {
				t.Fatalf("error = %v, want detail %q", err, test.detail)
			}
		})
	}
}

func TestLoad_KnowledgeRejectsUnknownKeyAndShape(t *testing.T) {
	for name, content := range map[string]string{
		"unknown knowledge key": "version: 1\nknowledge:\n  reranker: x\n",
		"knowledge scalar":      "version: 1\nknowledge: on\n",
		"embedding scalar":      "version: 1\nknowledge:\n  embedding: on\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeDiscordConfig(t, content)); err == nil {
				t.Fatal("Load accepted an invalid knowledge section")
			}
		})
	}
}

func TestLoad_KnowledgeSectionKeepsVersionGateFirst(t *testing.T) {
	_, err := Load(writeDiscordConfig(t, "version: 2\nknowledge:\n  embedding:\n    bogus: 1\n"))
	if code, ok := CodeOf(err); !ok || code != ErrorCodeVersionUnsupported {
		t.Fatalf("error = %v, want the version gate", err)
	}
}
