package tool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/yhwyxy/AgentNexus/internal/mcpclient"
	"github.com/yhwyxy/AgentNexus/internal/server"
)

func NormalizeTools(serverID server.ID, serverName string, tools []mcpclient.Tool, newID func() string) ([]Definition, string, error) {
	definitions := make([]Definition, 0, len(tools))
	backendNames := make(map[string]struct{}, len(tools))
	publicNames := make(map[string]struct{}, len(tools))
	for _, remote := range tools {
		if remote.Name == "" {
			return nil, "", fmt.Errorf("%w: tool name is empty", ErrInvalidTool)
		}
		if _, exists := backendNames[remote.Name]; exists {
			return nil, "", fmt.Errorf("%w: duplicate backend tool name %q", ErrInvalidTool, remote.Name)
		}
		backendNames[remote.Name] = struct{}{}
		publicName := serverName + "." + remote.Name
		if len(publicName) > 128 || !validName(publicName) {
			return nil, "", fmt.Errorf("%w: invalid public tool name %q", ErrInvalidTool, publicName)
		}
		if _, exists := publicNames[publicName]; exists {
			return nil, "", fmt.Errorf("%w: duplicate public tool name %q", ErrInvalidTool, publicName)
		}
		publicNames[publicName] = struct{}{}

		input, err := canonicalJSON(remote.InputSchema, true)
		if err != nil {
			return nil, "", fmt.Errorf("%w: tool %q input schema: %v", ErrInvalidTool, remote.Name, err)
		}
		inputDigest := sha256.Sum256(input)
		var output json.RawMessage
		if len(remote.OutputSchema) != 0 && !bytes.Equal(bytes.TrimSpace(remote.OutputSchema), []byte("null")) {
			output, err = canonicalJSON(remote.OutputSchema, false)
			if err != nil {
				return nil, "", fmt.Errorf("%w: tool %q output schema: %v", ErrInvalidTool, remote.Name, err)
			}
		}
		annotations := json.RawMessage(`{}`)
		if len(remote.Annotations) != 0 && !bytes.Equal(bytes.TrimSpace(remote.Annotations), []byte("null")) {
			annotations, err = canonicalJSON(remote.Annotations, false)
			if err != nil {
				return nil, "", fmt.Errorf("%w: tool %q annotations: %v", ErrInvalidTool, remote.Name, err)
			}
		}
		definitions = append(definitions, Definition{
			ID: newID(), ServerID: serverID, ServerName: serverName, BackendName: remote.Name,
			PublicName: publicName, Title: remote.Title, Description: remote.Description,
			InputSchema: input, OutputSchema: output, Annotations: annotations,
			SchemaDigest: hex.EncodeToString(inputDigest[:]),
		})
	}
	return definitions, catalogDigest(definitions), nil
}

func canonicalJSON(raw []byte, requireObject bool) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("schema is missing")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	if requireObject {
		if _, ok := value.(map[string]any); !ok {
			return nil, fmt.Errorf("must be a JSON object")
		}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("contains multiple JSON values")
		}
		return nil, fmt.Errorf("trailing JSON: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode canonical JSON: %w", err)
	}
	return canonical, nil
}

func validName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.-", rune(c)) {
			continue
		}
		return false
	}
	return true
}

type digestDefinition struct {
	PublicName   string          `json:"publicName"`
	BackendName  string          `json:"backendName"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  json.RawMessage `json:"annotations"`
}

func catalogDigest(definitions []Definition) string {
	ordered := append([]Definition(nil), definitions...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PublicName < ordered[j].PublicName })
	entries := make([]digestDefinition, 0, len(ordered))
	for _, definition := range ordered {
		entries = append(entries, digestDefinition{
			PublicName: definition.PublicName, BackendName: definition.BackendName,
			Title: definition.Title, Description: definition.Description,
			InputSchema: definition.InputSchema, OutputSchema: definition.OutputSchema,
			Annotations: definition.Annotations,
		})
	}
	encoded, _ := json.Marshal(entries)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
