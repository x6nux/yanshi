# v1 JSON Schema

> 以下为 `sdk/schema/v1/agent-api.schema.json` 的完整 JSON Schema，由
> `go run ./cmd/api-schema -markdown` 经 `internal/api/v1/schema.go::SchemaBytes` 生成 ——
> 它返回的就是那个文件本身（`sdk/schema/schema.go::V1`），所以这两句同时为真。
> 修改 schema 后重生成；不要手改本区块。

<!-- BEGIN GENERATED: api-schema-full -->
```json
{
  "$defs": {
    "ApiErrorBody": {
      "additionalProperties": true,
      "properties": {
        "code": {
          "type": "string"
        },
        "message": {
          "minLength": 1,
          "type": "string"
        },
        "retryable": {
          "type": "boolean"
        }
      },
      "required": [
        "message"
      ],
      "type": "object"
    },
    "Capabilities": {
      "additionalProperties": true,
      "properties": {
        "itemTypes": {
          "items": {
            "type": "string"
          },
          "type": "array"
        },
        "methods": {
          "items": {
            "type": "string"
          },
          "type": "array"
        },
        "stream": {
          "const": "item/updated"
        },
        "unknownFields": {
          "const": "ignored"
        },
        "version": {
          "$ref": "#/$defs/Version"
        }
      },
      "required": [
        "version",
        "methods",
        "itemTypes",
        "unknownFields",
        "stream"
      ],
      "type": "object"
    },
    "ContextItem": {
      "description": "D2-provisional IDE context. Not yet emitted by D1; the SDK accepts it on turn/start and forwards as context.",
      "oneOf": [
        {
          "additionalProperties": true,
          "properties": {
            "content": {
              "type": "string"
            },
            "kind": {
              "const": "selection"
            },
            "path": {
              "minLength": 1,
              "type": "string"
            },
            "range": {
              "$ref": "#/$defs/Range"
            },
            "truncated": {
              "type": "boolean"
            }
          },
          "required": [
            "kind",
            "path",
            "content",
            "range"
          ],
          "type": "object"
        },
        {
          "additionalProperties": true,
          "properties": {
            "content": {
              "type": "string"
            },
            "kind": {
              "const": "openFile"
            },
            "path": {
              "minLength": 1,
              "type": "string"
            },
            "truncated": {
              "type": "boolean"
            }
          },
          "required": [
            "kind",
            "path",
            "content"
          ],
          "type": "object"
        }
      ]
    },
    "FileChange": {
      "additionalProperties": true,
      "description": "D2-provisional. D1 does not yet emit fileChange items; the IDE diff renderer degrades gracefully when this field is absent.",
      "properties": {
        "after": {
          "type": "string"
        },
        "before": {
          "type": "string"
        },
        "path": {
          "minLength": 1,
          "type": "string"
        },
        "unifiedDiff": {
          "type": "string"
        }
      },
      "required": [
        "path"
      ],
      "type": "object"
    },
    "ImageAttach": {
      "additionalProperties": true,
      "properties": {
        "dataB64": {
          "type": "string"
        },
        "fmt": {
          "type": "string"
        },
        "h": {
          "minimum": 0,
          "type": "integer"
        },
        "id": {
          "type": "string"
        },
        "source": {
          "type": "string"
        },
        "w": {
          "minimum": 0,
          "type": "integer"
        }
      },
      "type": "object"
    },
    "InterruptResponse": {
      "additionalProperties": true,
      "properties": {
        "ok": {
          "type": "boolean"
        },
        "threadId": {
          "minLength": 1,
          "type": "string"
        },
        "turnId": {
          "type": "string"
        },
        "version": {
          "$ref": "#/$defs/Version"
        }
      },
      "required": [
        "version",
        "ok",
        "threadId"
      ],
      "type": "object"
    },
    "Item": {
      "additionalProperties": true,
      "properties": {
        "error": {
          "type": "string"
        },
        "fileChange": {
          "$ref": "#/$defs/FileChange"
        },
        "id": {
          "minLength": 1,
          "type": "string"
        },
        "sequence": {
          "description": "Monotonic per-thread counter starting at 1. Used for reconnect replay and de-duplication.",
          "minimum": 1,
          "type": "integer"
        },
        "status": {
          "type": "string"
        },
        "structuredResult": {},
        "text": {
          "type": "string"
        },
        "threadId": {
          "minLength": 1,
          "type": "string"
        },
        "toolArgs": {
          "type": "string"
        },
        "toolName": {
          "type": "string"
        },
        "turnId": {
          "minLength": 1,
          "type": "string"
        },
        "type": {
          "minLength": 1,
          "type": "string"
        },
        "version": {
          "$ref": "#/$defs/Version"
        }
      },
      "required": [
        "version",
        "id",
        "sequence",
        "threadId",
        "turnId",
        "type"
      ],
      "type": "object"
    },
    "ItemType": {
      "description": "D1 known item types. Unknown server values arrive as 'event.\u003clegacyType\u003e'; SDK normalizes them to 'unknown' but preserves the original type string alongside.",
      "enum": [
        "turn.started",
        "message.delta",
        "reasoning.delta",
        "tool.call",
        "tool.result",
        "tool.progress",
        "structured.result",
        "turn.error",
        "turn.completed"
      ],
      "type": "string"
    },
    "Range": {
      "additionalProperties": false,
      "properties": {
        "end": {
          "minimum": 0,
          "type": "integer"
        },
        "start": {
          "minimum": 0,
          "type": "integer"
        }
      },
      "required": [
        "start",
        "end"
      ],
      "type": "object"
    },
    "Thread": {
      "additionalProperties": true,
      "properties": {
        "createdAt": {
          "description": "Unix seconds.",
          "minimum": 0,
          "type": "integer"
        },
        "id": {
          "minLength": 1,
          "type": "string"
        },
        "model": {
          "type": "string"
        },
        "status": {
          "$ref": "#/$defs/ThreadStatus"
        },
        "thinking": {
          "type": "string"
        },
        "title": {
          "type": "string"
        },
        "turns": {
          "items": {
            "$ref": "#/$defs/Turn"
          },
          "type": "array"
        },
        "updatedAt": {
          "minimum": 0,
          "type": "integer"
        },
        "version": {
          "$ref": "#/$defs/Version"
        }
      },
      "required": [
        "version",
        "id",
        "status",
        "createdAt",
        "updatedAt"
      ],
      "type": "object"
    },
    "ThreadInterruptParams": {
      "additionalProperties": false,
      "properties": {
        "threadId": {
          "minLength": 1,
          "type": "string"
        },
        "turnId": {
          "type": "string"
        },
        "version": {
          "$ref": "#/$defs/Version"
        }
      },
      "required": [
        "threadId"
      ],
      "type": "object"
    },
    "ThreadResumeParams": {
      "additionalProperties": false,
      "properties": {
        "threadId": {
          "minLength": 1,
          "type": "string"
        },
        "version": {
          "$ref": "#/$defs/Version"
        }
      },
      "required": [
        "threadId"
      ],
      "type": "object"
    },
    "ThreadResumeResponse": {
      "additionalProperties": true,
      "properties": {
        "thread": {
          "$ref": "#/$defs/Thread"
        },
        "version": {
          "$ref": "#/$defs/Version"
        }
      },
      "required": [
        "version",
        "thread"
      ],
      "type": "object"
    },
    "ThreadStartParams": {
      "additionalProperties": true,
      "properties": {
        "model": {
          "type": "string"
        },
        "thinking": {
          "type": "string"
        },
        "title": {
          "type": "string"
        },
        "version": {
          "$ref": "#/$defs/Version"
        }
      },
      "type": "object"
    },
    "ThreadStartResponse": {
      "additionalProperties": true,
      "properties": {
        "thread": {
          "$ref": "#/$defs/Thread"
        },
        "version": {
          "$ref": "#/$defs/Version"
        }
      },
      "required": [
        "version",
        "thread"
      ],
      "type": "object"
    },
    "ThreadStatus": {
      "enum": [
        "active",
        "archived"
      ],
      "type": "string"
    },
    "Turn": {
      "additionalProperties": true,
      "properties": {
        "completedAt": {
          "minimum": 0,
          "type": "integer"
        },
        "id": {
          "minLength": 1,
          "type": "string"
        },
        "input": {
          "type": "string"
        },
        "startedAt": {
          "minimum": 0,
          "type": "integer"
        },
        "status": {
          "$ref": "#/$defs/TurnStatus"
        },
        "threadId": {
          "minLength": 1,
          "type": "string"
        },
        "version": {
          "$ref": "#/$defs/Version"
        }
      },
      "required": [
        "version",
        "id",
        "threadId",
        "status",
        "input",
        "startedAt"
      ],
      "type": "object"
    },
    "TurnStartParams": {
      "additionalProperties": true,
      "properties": {
        "context": {
          "items": {
            "$ref": "#/$defs/ContextItem"
          },
          "maxItems": 16,
          "type": "array"
        },
        "images": {
          "items": {
            "$ref": "#/$defs/ImageAttach"
          },
          "type": "array"
        },
        "input": {
          "minLength": 1,
          "type": "string"
        },
        "model": {
          "type": "string"
        },
        "outputSchema": {},
        "thinking": {
          "type": "string"
        },
        "threadId": {
          "minLength": 1,
          "type": "string"
        },
        "version": {
          "$ref": "#/$defs/Version"
        }
      },
      "required": [
        "threadId",
        "input"
      ],
      "type": "object"
    },
    "TurnStartResponse": {
      "additionalProperties": true,
      "properties": {
        "turn": {
          "$ref": "#/$defs/Turn"
        },
        "version": {
          "$ref": "#/$defs/Version"
        }
      },
      "required": [
        "version",
        "turn"
      ],
      "type": "object"
    },
    "TurnStatus": {
      "enum": [
        "inProgress",
        "completed",
        "interrupted",
        "failed"
      ],
      "type": "string"
    },
    "Version": {
      "const": "v1",
      "description": "D1 currently emits exactly 'v1'; clients remain tolerant of future 'v1.x' minors.",
      "type": "string"
    }
  },
  "$id": "https://yanshi.dev/schema/agent-api/v1/agent-api.schema.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "additionalProperties": true,
  "anyOf": [
    {
      "$ref": "#/$defs/ThreadStartResponse"
    },
    {
      "$ref": "#/$defs/ThreadResumeResponse"
    },
    {
      "$ref": "#/$defs/TurnStartResponse"
    },
    {
      "$ref": "#/$defs/InterruptResponse"
    },
    {
      "$ref": "#/$defs/Item"
    },
    {
      "$ref": "#/$defs/Capabilities"
    }
  ],
  "description": "The v1 Agent API contract. This file IS the canonical schema: internal/api/v1.SchemaBytes embeds it and GET /api/v1/schema/agent-v1.json serves these bytes verbatim, so the document a client fetches and the document its SDK validates against are one artifact. It also carries D2-provisional IDE-context extensions (ContextItem, FileChange, Range) that have no Go counterpart by design — see sdk/schema/CONTRACT_HANDOFF.md and the intentional-difference table in internal/api/v1/parity_test.go.",
  "title": "Yanshi Agent API v1",
  "type": "object"
}
```
<!-- END GENERATED: api-schema-full -->
