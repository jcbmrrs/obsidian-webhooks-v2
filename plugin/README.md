# Obsidian Webhooks v2 Plugin

This plugin connects to the Obsidian Webhooks v2 Go server to receive real-time webhook events and create/update notes in your vault.

## Development

### Prerequisites

Make sure you have `mise` installed and configured:

```bash
# From the v2 directory
mise install
```

This will install:
- Go 1.24.4
- Bun (latest)

### Building

```bash
# Install dependencies
bun install

# Build for production
bun run build

# Development with watch mode
bun run dev
```

### Installation

1. Build the plugin using the steps above
2. Copy the entire `plugin/` folder to your Obsidian vault's `.obsidian/plugins/` directory
3. Rename it to something like `obsidian-webhooks-v2`
4. Enable the plugin in Obsidian Settings → Community Plugins

### Configuration

1. Make sure your Go server is running
2. In Obsidian Settings → Webhooks v2:
   - Set the server endpoint (e.g., `http://localhost:8080`)
   - Enter your API key from the server dashboard
   - Configure newline preferences

### Testing

Use the "Test Connection" button in the plugin settings to verify everything is working.

## Features

- Real-time webhook processing via Server-Sent Events
- Automatic file/folder creation
- Configurable newline handling
- Connection status monitoring
- Built-in test functionality

## Files

- `main.ts` - Plugin source code
- `manifest.json` - Plugin metadata
- `package.json` - Build configuration and dependencies
- `tsconfig.json` - TypeScript settings
- `versions.json` - Obsidian compatibility matrix