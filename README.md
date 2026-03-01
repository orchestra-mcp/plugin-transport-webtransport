# Orchestra Plugin: transport-webtransport

A transport plugin for the [Orchestra MCP](https://github.com/orchestra-mcp/framework) framework. Provides an HTTP/2 JSON-RPC 2.0 gateway serving browser clients and embedded React dashboard.

## Install

```bash
go install github.com/orchestra-mcp/plugin-transport-webtransport/cmd@latest
```

## Usage

Add to your `plugins.yaml`:

```yaml
- id: transport.webtransport
  binary: ./bin/transport-webtransport
  enabled: true
```

## Related Packages

- [sdk-go](https://github.com/orchestra-mcp/sdk-go) — Plugin SDK
- [gen-go](https://github.com/orchestra-mcp/gen-go) — Generated Protobuf types
