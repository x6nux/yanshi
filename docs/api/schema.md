# v1 JSON Schema

> 以下为 `sdk/schema/v1/agent-api.schema.json` 的完整 JSON Schema，由
> `go run ./cmd/api-schema -markdown` 从 `internal/api/v1/schema.go::schemaDocument` 生成。
> 修改 schema 后重生成；不要手改本区块。

<!-- BEGIN GENERATED: api-schema-full -->
```json
{
  "$defs": {
    "Item": {
      "properties": {
        "error": {
          "type": "string"
        },
        "id": {
          "type": "string"
        },
        "sequence": {
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
          "type": "string"
        },
        "toolArgs": {
          "type": "string"
        },
        "toolName": {
          "type": "string"
        },
        "turnId": {
          "type": "string"
        },
        "type": {
          "type": "string"
        },
        "version": {
          "const": "v1"
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
    "Thread": {
      "properties": {
        "createdAt": {
          "type": "integer"
        },
        "id": {
          "type": "string"
        },
        "model": {
          "type": "string"
        },
        "status": {
          "type": "string"
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
          "type": "integer"
        },
        "version": {
          "const": "v1"
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
    "Turn": {
      "properties": {
        "completedAt": {
          "type": "integer"
        },
        "id": {
          "type": "string"
        },
        "input": {
          "type": "string"
        },
        "startedAt": {
          "type": "integer"
        },
        "status": {
          "type": "string"
        },
        "threadId": {
          "type": "string"
        },
        "version": {
          "const": "v1"
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
    }
  },
  "$id": "https://yanshi.dev/schema/agent-api-v1.json",
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "Yanshi Agent API v1",
  "type": "object"
}
```
<!-- END GENERATED: api-schema-full -->
