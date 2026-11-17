package schemas

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// exactCanonical never converts JSON numbers through float64. This is also the
// historical B7-07 fingerprint's canonicalization contract.
func exactCanonical(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
func digestProjection(v any) string {
	b, err := exactCanonical(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// MCPDefinitionFingerprint covers the entire typed view (including MCP Apps
// visibility), except the authoritative gateway name and the internal carrier.
func MCPDefinitionFingerprint(tool ChatTool) string {
	tool.InvalidateSerialized()
	if tool.Function != nil {
		f := *tool.Function
		f.Name = ""
		tool.Function = &f
	}
	var metadata *MCPToolMetadata
	if tool.MCP != nil {
		m := *tool.MCP
		m.RawDefinition = nil
		m.ProjectionFingerprint = ""
		metadata = &m
	}
	return digestProjection(struct {
		Tool        ChatTool            `json:"tool"`
		Annotations *MCPToolAnnotations `json:"annotations"`
		Metadata    *MCPToolMetadata    `json:"metadata"`
	}{tool, tool.Annotations, metadata})
}

type lighthouseLegacyMetadata struct {
	Version     int                 `json:"version"`
	Schema      json.RawMessage     `json:"schema,omitempty"`
	Annotations *MCPToolAnnotations `json:"annotations,omitempty"`
	Raw         json.RawMessage     `json:"sdkRawToolJSON,omitempty"`
	Fingerprint string              `json:"sdkRawToolProjectionFingerprint,omitempty"`
}

// legacyFingerprint validates the old cache before translating it. Use the
// persisted projection bytes, avoiding lossy decoding by the new typed model.
func legacyFingerprint(fields map[string]json.RawMessage, meta lighthouseLegacyMetadata) (string, error) {
	var fn map[string]json.RawMessage
	if err := json.Unmarshal(fields["function"], &fn); err != nil {
		return "", err
	}
	present := func(b json.RawMessage) bool { return len(b) > 0 && string(b) != "null" }
	p := map[string]any{
		"type": fields["type"], "function_present": fn != nil,
		"description_present": present(fn["description"]), "parameters_present": present(fn["parameters"]),
		"strict_present": present(fn["strict"]), "schema_present": present(meta.Schema),
		"annotations_present": meta.Annotations != nil,
	}
	for _, k := range []string{"description", "parameters", "strict"} {
		if present(fn[k]) {
			p[k] = fn[k]
		}
	}
	if present(meta.Schema) {
		p["schema"] = meta.Schema
	}
	if meta.Annotations != nil {
		p["annotations"] = meta.Annotations
	}
	return digestProjection(p), nil
}

// DecodeLighthouseToolCache accepts normal upstream caches and validates the
// deployed v1/v2 format before moving its metadata into the native details path.
func DecodeLighthouseToolCache(data []byte, tools *map[string]ChatTool) error {
	if err := json.Unmarshal(data, tools); err != nil {
		return err
	}
	var objects map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &objects); err != nil {
		return err
	}
	for name, fields := range objects {
		raw, exists := fields["_bifrost_mcp"]
		if !exists {
			continue
		}
		var old lighthouseLegacyMetadata
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&old); err != nil {
			return fmt.Errorf("legacy cache %q: %w", name, err)
		}
		if old.Version != 1 && old.Version != 2 {
			return fmt.Errorf("unsupported legacy cache version %d", old.Version)
		}
		tool := (*tools)[name]
		if tool.Function == nil {
			return fmt.Errorf("legacy tool %q has no function", name)
		}
		if old.Version == 2 {
			fp, err := legacyFingerprint(fields, old)
			if err != nil || fp == "" || fp != old.Fingerprint {
				return fmt.Errorf("legacy tool %q projection conflict", name)
			}
			var upstream mcp.Tool
			if len(old.Raw) == 0 || json.Unmarshal(old.Raw, &upstream) != nil || upstream.Name == "" {
				return fmt.Errorf("invalid legacy raw tool %q", name)
			}
			if tool.Function.Name != upstream.Name && !strings.HasSuffix(tool.Function.Name, "-"+upstream.Name) {
				return fmt.Errorf("legacy tool %q name mismatch", name)
			}
			var def map[string]json.RawMessage
			if err := json.Unmarshal(old.Raw, &def); err != nil {
				return err
			}
			tool.MCP = &MCPToolMetadata{Title: upstream.Title, Icons: upstream.Icons, OutputSchema: def["outputSchema"], Meta: def["_meta"], RawDefinition: append(json.RawMessage(nil), old.Raw...)}
			// The metadata parser and Apps visibility are shared with fresh discovery
			// through RestoreMCPAppsMetadata, including legacy ui/resourceUri handling.
			RestoreMCPAppsMetadata(tool.MCP)
		} else {
			if len(old.Raw) > 0 || old.Fingerprint != "" {
				return fmt.Errorf("v1 cache contains v2 fields")
			}
			var schema map[string]json.RawMessage
			if len(old.Schema) > 0 && json.Unmarshal(old.Schema, &schema) != nil {
				return fmt.Errorf("invalid legacy schema")
			}
			// v1 has no full raw carrier. Preserve its schemas with an explicit
			// reconstructed definition, without inventing upstream metadata.
			def := map[string]any{"name": tool.Function.Name, "inputSchema": schema["inputSchema"]}
			if tool.Function.Description != nil {
				def["description"] = *tool.Function.Description
			}
			if len(schema["outputSchema"]) > 0 {
				def["outputSchema"] = schema["outputSchema"]
			}
			if old.Annotations != nil {
				def["annotations"] = old.Annotations
			}
			b, err := json.Marshal(def)
			if err != nil {
				return err
			}
			tool.MCP = &MCPToolMetadata{OutputSchema: schema["outputSchema"], RawDefinition: b}
		}
		tool.Annotations = old.Annotations
		tool.MCP.ProjectionFingerprint = MCPDefinitionFingerprint(tool)
		(*tools)[name] = tool
	}
	return nil
}

// RestoreMCPAppsMetadata applies the same visibility rules to legacy caches as
// current discovery. Raw metadata is not an authorization grant.
func RestoreMCPAppsMetadata(m *MCPToolMetadata) {
	var fields struct {
		UI struct {
			ResourceURI string   `json:"resourceUri"`
			Visibility  []string `json:"visibility"`
		} `json:"ui"`
		LegacyURI string `json:"ui/resourceUri"`
	}
	if json.Unmarshal(m.Meta, &fields) != nil {
		return
	}
	m.UIResourceURI = fields.UI.ResourceURI
	if m.UIResourceURI == "" {
		m.UIResourceURI = fields.LegacyURI
	}
	if !strings.HasPrefix(m.UIResourceURI, "ui://") {
		m.UIResourceURI = ""
	}
	for _, v := range fields.UI.Visibility {
		if v == "model" || v == "app" {
			m.Visibility = append(m.Visibility, v)
		}
	}
}
