package cli

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// YAML surgery for `yanshi provider add`.
//
// The alternative — unmarshal into config.Config, mutate, re-marshal — was
// rejected because config.example.yaml is roughly half comments, and those
// comments are the actual documentation an operator reads while editing. A
// struct round-trip erases all of them and reorders the keys, so a command that
// adds four lines would rewrite the whole file into something the operator no
// longer recognises. yaml.Node keeps comments, order and formatting.

// upsertProvider inserts or replaces spec in doc's llm.providers sequence.
// Reports whether an existing entry of that name was replaced.
func upsertProvider(doc *yaml.Node, spec providerSpec) (replaced bool, err error) {
	seq := providerSequence(doc, true)
	if seq == nil {
		return false, fmt.Errorf("the config has no llm.providers list and one could not be created")
	}
	entry := providerNode(spec)
	for i, existing := range seq.Content {
		if mappingValue(existing, "name") != spec.Name {
			continue
		}
		// Replace the whole node rather than patching fields. A partial patch
		// leaves the previous model / base_url / api_key of a provider the
		// operator explicitly asked to replace, which is the opposite of what
		// "replace" means and produces a hybrid nobody wrote.
		seq.Content[i] = entry
		return true, nil
	}
	seq.Content = append(seq.Content, entry)
	return false, nil
}

// providerSequence locates the llm.providers sequence node, creating the llm
// mapping and the providers key when create is true.
//
// It returns nil rather than an error for a document that is not a mapping —
// an empty file, or one holding a bare scalar. The caller turns that into the
// "no llm.providers list" message, which is more useful than "expected
// !!map".
func providerSequence(doc *yaml.Node, create bool) *yaml.Node {
	root := documentRoot(doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil
	}
	llm := mappingChild(root, "llm")
	if llm == nil {
		if !create {
			return nil
		}
		llm = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendMapping(root, "llm", llm)
	}
	if llm.Kind != yaml.MappingNode {
		return nil
	}
	providers := mappingChild(llm, "providers")
	if providers == nil {
		if !create {
			return nil
		}
		providers = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		appendMapping(llm, "providers", providers)
		return providers
	}
	if providers.Kind == yaml.SequenceNode {
		return providers
	}
	// `providers:` written with no value parses as a null scalar, which is a
	// perfectly ordinary thing for an operator to leave behind. Promote it
	// rather than refusing.
	if providers.Kind == yaml.ScalarNode && providers.Tag == "!!null" && create {
		providers.Kind = yaml.SequenceNode
		providers.Tag = "!!seq"
		providers.Value = ""
		return providers
	}
	return nil
}

// documentRoot unwraps a DocumentNode to its single content node.
func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		return doc.Content[0]
	}
	return doc
}

// mappingChild returns the value node for key in a mapping, or nil.
func mappingChild(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	// Mapping content alternates key, value — hence the stride of two and the
	// i+1 bound check, which matters for a truncated document.
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mappingValue returns the scalar value for key in a mapping, or "".
func mappingValue(m *yaml.Node, key string) string {
	child := mappingChild(m, key)
	if child == nil || child.Kind != yaml.ScalarNode {
		return ""
	}
	return child.Value
}

// appendMapping adds key: value to a mapping node.
func appendMapping(m *yaml.Node, key string, value *yaml.Node) {
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

// providerNode builds the YAML mapping for one provider entry.
//
// api_key holds a secret:// REFERENCE, never the key. That is the whole reason
// this command exists rather than telling the operator to edit the file.
// base_url and context_window are omitted when unset so the adapter default and
// the built-in model catalog stay in force — writing an empty string or a zero
// would override them with a value nobody chose.
func providerNode(spec providerSpec) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	appendMapping(n, "name", scalar(spec.Name))
	appendMapping(n, "kind", scalar(spec.Kind))
	appendMapping(n, "model", scalar(spec.Model))
	appendMapping(n, "api_key", scalar(ProviderSecretRef(spec.Name)))
	if spec.BaseURL != "" {
		appendMapping(n, "base_url", scalar(spec.BaseURL))
	}
	if cw := providerContextWindowNode(spec.ContextWindow); cw != nil {
		appendMapping(n, "context_window", cw)
	}
	return n
}

// scalar builds a quoted string scalar. Quoting is explicit so a model id that
// looks like a YAML number or boolean ("1.5", "on") survives the round trip as
// the string it is.
func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.DoubleQuotedStyle, Value: v}
}
