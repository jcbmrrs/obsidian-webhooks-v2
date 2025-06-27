# Obsidian Webhooks v2

A minimal Go server and Obsidian plugin for receiving webhooks and creating/updating notes in your vault.

## Why v2?

v2 is a complete rewrite addressing key limitations of v1:

- **Self-hosted**: v1 relied on Firebase hosting which is difficult to self-host and maintain
- **Abandoned codebase**: The original v1 codebase became abandonware, making it hard to maintain or extend
- **External dependencies**: v1 required external web services, while v2 is completely self-contained

v2 provides a robust, maintainable solution that you can run entirely on your own infrastructure.

## Shared Hosting Service

A shared hosting service is coming soon for users who prefer not to self-host. This will provide the convenience of v1's hosted service with v2's improved architecture and reliability. Stay tuned for deployment updates.

## Features

- **Single Binary**: Everything runs from one Go executable
- **Real-time Updates**: Server-Sent Events (SSE) for instant note updates
- **Zero-Downtime Deploys**: Hot binary swapping with connection transfer
- **Simple Auth**: Environment variable auth for single-user or Google OAuth for multi-user
- **No JavaScript**: Web UI uses only server-side HTML with Tailwind CSS
- **Embedded Database**: BoltDB for zero external dependencies

## Quick Start

### Server Setup

1. Build and run the server:

```bash
go build -o webhooks-server main.go
AUTH_TOKEN=your-secret-token ./webhooks-server
```

2. Visit http://localhost:8080 and login with your token

3. Generate an API key from the dashboard

### Zero-Downtime Deployment

The server supports hot binary swapping for zero-downtime deployments:

```bash
# Build new version
go build -o webhooks-server-new main.go

# Replace binary (server keeps running)
mv webhooks-server webhooks-server-old
mv webhooks-server-new webhooks-server

# Trigger hot reload (zero downtime!)
kill -USR2 $(pgrep webhooks-server)
```

**What happens:**

- New process starts with updated binary
- HTTP connections transfer seamlessly
- SSE clients reconnect automatically (~1-2 seconds)
- Database handoff is coordinated
- Old process shuts down gracefully

### Obsidian Plugin

1. Run `mise run plugin-build` to build the plugin
1. Copy the `plugin` folder to `.obsidian/plugins/obsidian-webhooks-v2/`
1. Enable the plugin in Obsidian settings
1. Enter your server URL and API key in plugin settings

## Usage

Send webhooks to create/append notes:

```bash
curl -X POST "http://localhost:8080/webhook/YOUR_API_KEY?path=daily/2024-01-01.md" \
  -H "Content-Type: text/plain" \
  -d "## 10:30 AM
- Meeting notes
- Action items"
```

## Environment Variables

- `PORT` - Server port (default: 8080)
- `AUTH_TOKEN` - Token for single-user auth
- `JWT_SECRET` - Secret for JWT signing (auto-generated if not set)

For Google OAuth:

- `AUTH_MODE=google`
- `GOOGLE_CLIENT_ID`
- `GOOGLE_CLIENT_SECRET`
- `GOOGLE_REDIRECT_URL`

## License

MIT
