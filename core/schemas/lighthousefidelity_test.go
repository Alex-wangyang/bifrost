package schemas

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestLighthouseFingerprintAndClone(t *testing.T) {
	raw := json.RawMessage(`{"name":"echo","inputSchema":{"type":"object","properties":{"n":{"enum":[9007199254740993]}}},"_meta":{"n":9007199254740993}}`)
	tool := ChatTool{Type: ChatToolTypeFunction, Function: &ChatToolFunction{Name: "echo", Description: Ptr("original"), Parameters: &ToolFunctionParameters{Type: "object", Properties: NewOrderedMap()}}, MCP: &MCPToolMetadata{RawDefinition: raw, Meta: json.RawMessage(`{"n":9007199254740993}`)}}
	tool.MCP.ProjectionFingerprint = MCPDefinitionFingerprint(tool)
	cloned := DeepCopyChatTool(tool)
	require.Equal(t, tool.MCP.ProjectionFingerprint, MCPDefinitionFingerprint(cloned))
	cloned.Function.Name = "client-echo"
	require.Equal(t, tool.MCP.ProjectionFingerprint, MCPDefinitionFingerprint(cloned))
	cloned.Function.Description = Ptr("filtered")
	require.NotEqual(t, tool.MCP.ProjectionFingerprint, MCPDefinitionFingerprint(cloned))
	cloned = DeepCopyChatTool(tool)
	cloned.MCP.Meta = json.RawMessage(`{"n":9007199254740992}`)
	require.NotEqual(t, tool.MCP.ProjectionFingerprint, MCPDefinitionFingerprint(cloned))
	cloned.MCP.RawDefinition[0] = 'x'
	require.Equal(t, byte('{'), tool.MCP.RawDefinition[0])
	b, err := json.Marshal(tool)
	require.NoError(t, err)
	require.NotContains(t, string(b), "raw_definition")
	require.NotContains(t, string(b), "9007199254740993")
}
func TestLighthouseLegacyCacheRejectsConflict(t *testing.T) {
	fields := map[string]json.RawMessage{"type": json.RawMessage(`"function"`), "function": json.RawMessage(`{"name":"client-echo","description":"original","parameters":{"type":"object","properties":{}}}`)}
	old := lighthouseLegacyMetadata{Version: 2, Schema: json.RawMessage(`{"inputSchema":{"type":"object"}}`), Raw: json.RawMessage(`{"name":"echo","description":"original","inputSchema":{"type":"object"}}`)}
	var err error
	old.Fingerprint, err = legacyFingerprint(fields, old)
	require.NoError(t, err)
	fields["_bifrost_mcp"], err = json.Marshal(old)
	require.NoError(t, err)
	data, _ := json.Marshal(map[string]any{"client-echo": fields})
	var tools map[string]ChatTool
	require.NoError(t, DecodeLighthouseToolCache(data, &tools))
	require.Equal(t, old.Raw, tools["client-echo"].MCP.RawDefinition)
	fields["function"] = json.RawMessage(`{"name":"client-echo","description":"changed","parameters":{"type":"object","properties":{}}}`)
	data, _ = json.Marshal(map[string]any{"client-echo": fields})
	require.ErrorContains(t, DecodeLighthouseToolCache(data, &tools), "projection conflict")
}
func TestLighthouseCapturedCaches(t *testing.T) {
	path := os.Getenv("LIGHTHOUSE_OFFLINE_CACHES")
	if path == "" {
		t.Skip("private offline cache not supplied")
	}
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var clients map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &clients))
	total := 0
	for name, data := range clients {
		var tools map[string]ChatTool
		require.NoError(t, DecodeLighthouseToolCache(data, &tools), name)
		for _, tool := range tools {
			require.NotNil(t, tool.MCP, name)
			require.NotEmpty(t, tool.MCP.RawDefinition, name)
			require.Equal(t, tool.MCP.ProjectionFingerprint, MCPDefinitionFingerprint(tool))
			total++
		}
	}
	require.Greater(t, total, 500)
	t.Logf("validated %d cached tools across %d clients", total, len(clients))
}
