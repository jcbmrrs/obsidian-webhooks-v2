import { App, Notice, Plugin, PluginSettingTab, Setting } from "obsidian";

interface WebhookEvent {
	id: string;
	user_id: string;
	path: string;
	data: string;
	timestamp: string;
}

interface MyPluginSettings {
	endpoint: string;
	clientKey: string;
	newlineType: "none" | "windows" | "unix";
	processedEventIds: string[];
}

const DEFAULT_SETTINGS: MyPluginSettings = {
	endpoint: "http://localhost:8080",
	clientKey: "",
	newlineType: "none",
	processedEventIds: [],
};

export default class ObsidianWebhooksPlugin extends Plugin {
	settings: MyPluginSettings;
	eventSource: EventSource | null = null;
	processedEvents: Set<string> = new Set();

	async onload() {
		console.log("loading obsidian-webhooks v2 plugin");
		await this.loadSettings();

		// Load persisted processed event IDs into the Set
		this.processedEvents = new Set(this.settings.processedEventIds);
		console.log(`Loaded ${this.processedEvents.size} processed event IDs from storage`);

		this.addSettingTab(new WebhookSettingTab(this.app, this));

		if (this.settings.clientKey) {
			this.connect();
		}
	}

	onunload() {
		console.log("unloading plugin");
		this.disconnect();
	}

	connect() {
		if (this.eventSource) {
			this.disconnect();
		}

		if (!this.settings.clientKey) {
			return;
		}

		const url = `${this.settings.endpoint}/events/${this.settings.clientKey}`;
		console.log("Connecting to SSE endpoint:", url);
		
		this.eventSource = new EventSource(url);
		
		this.eventSource.onmessage = async (event) => {
			try {
				const webhookEvent: WebhookEvent = JSON.parse(event.data);

				// Skip if we've already processed this event
				if (this.processedEvents.has(webhookEvent.id)) {
					console.log(`Skipping already processed event: ${webhookEvent.id}`);
					return;
				}

				await this.applyEvent(webhookEvent);
				this.processedEvents.add(webhookEvent.id);

				// Persist the updated set (with size management)
				await this.persistProcessedEvents();

			} catch (err) {
				console.error("Error processing webhook event:", err);
				new Notice("Error processing webhook event: " + err.message);
			}
		};
		
		this.eventSource.onerror = (error) => {
			console.error("SSE connection error:", error);
			new Notice("Webhook connection error - check settings");
		};
		
		this.eventSource.onopen = () => {
			console.log("SSE connection established");
			new Notice("Webhook connection established");
		};
	}

	disconnect() {
		if (this.eventSource) {
			this.eventSource.close();
			this.eventSource = null;
		}
	}

	async applyEvent(event: WebhookEvent) {
		const { path, data } = event;
		
		// Ensure directory exists
		const dirPath = path.replace(/\/[^\/]*$/, "");
		if (dirPath && dirPath !== path) {
			try {
				await this.app.vault.createFolder(dirPath);
			} catch (err) {
				// Folder might already exist
			}
		}
		
		// Add newline if configured
		let contentToSave = data;
		if (this.settings.newlineType === "unix") {
			contentToSave += "\n";
		} else if (this.settings.newlineType === "windows") {
			contentToSave += "\r\n";
		}
		
		// Check if file exists
		try {
			const existingFile = this.app.vault.getAbstractFileByPath(path);
			if (existingFile && existingFile.hasOwnProperty("extension")) {
				// File exists, append to it
				const existingContent = await this.app.vault.read(existingFile as any);
				contentToSave = existingContent + contentToSave;
				await this.app.vault.modify(existingFile as any, contentToSave);
			} else {
				// Create new file
				await this.app.vault.create(path, contentToSave);
			}
			
			console.log("Applied webhook event to:", path);
		} catch (err) {
			console.error("Error applying event:", err);
			throw err;
		}
	}

	async persistProcessedEvents() {
		// Keep set size reasonable (keep last 1000 events)
		if (this.processedEvents.size > 1000) {
			const entries = Array.from(this.processedEvents);
			this.processedEvents = new Set(entries.slice(-1000));
		}

		// Save to settings
		this.settings.processedEventIds = Array.from(this.processedEvents);
		await this.saveSettings();
	}

	async loadSettings() {
		this.settings = Object.assign({}, DEFAULT_SETTINGS, await this.loadData());
	}

	async saveSettings() {
		await this.saveData(this.settings);
	}
}

class WebhookSettingTab extends PluginSettingTab {
	plugin: ObsidianWebhooksPlugin;

	constructor(app: App, plugin: ObsidianWebhooksPlugin) {
		super(app, plugin);
		this.plugin = plugin;
	}

	display(): void {
		const { containerEl } = this;
		containerEl.empty();

		containerEl.createEl("h2", { text: "Obsidian Webhooks Settings" });

		new Setting(containerEl)
			.setName("Server Endpoint")
			.setDesc("The webhook server URL (e.g., https://your-server.com)")
			.addText((text) =>
				text
					.setPlaceholder("http://localhost:8080")
					.setValue(this.plugin.settings.endpoint)
					.onChange(async (value) => {
						this.plugin.settings.endpoint = value;
						await this.plugin.saveSettings();
					})
			);

		new Setting(containerEl)
			.setName("Client Key")
			.setDesc("Your client key from the server dashboard (read-only key)")
			.addText((text) =>
				text
					.setPlaceholder("Enter your client key (cl_...)")
					.setValue(this.plugin.settings.clientKey)
					.onChange(async (value) => {
						this.plugin.settings.clientKey = value;
						await this.plugin.saveSettings();
						
						// Reconnect with new key
						if (value) {
							this.plugin.connect();
						} else {
							this.plugin.disconnect();
						}
					})
			);

		new Setting(containerEl)
			.setName("New Line Type")
			.setDesc("Add newlines between incoming notes")
			.addDropdown((dropdown) => {
				dropdown
					.addOption("none", "No newlines")
					.addOption("windows", "Windows style (\\r\\n)")
					.addOption("unix", "Unix/Mac style (\\n)")
					.setValue(this.plugin.settings.newlineType)
					.onChange(async (value: "none" | "windows" | "unix") => {
						this.plugin.settings.newlineType = value;
						await this.plugin.saveSettings();
					});
			});

		const statusDiv = containerEl.createDiv("webhook-status");
		statusDiv.style.marginTop = "20px";
		statusDiv.style.padding = "10px";
		statusDiv.style.borderRadius = "5px";
		
		if (this.plugin.eventSource && this.plugin.eventSource.readyState === EventSource.OPEN) {
			statusDiv.style.backgroundColor = "#d4edda";
			statusDiv.style.color = "#155724";
			statusDiv.setText("✓ Connected to webhook server");
		} else if (this.plugin.settings.apiKey) {
			statusDiv.style.backgroundColor = "#f8d7da";
			statusDiv.style.color = "#721c24";
			statusDiv.setText("✗ Not connected - check server and API key");
		} else {
			statusDiv.style.backgroundColor = "#d1ecf1";
			statusDiv.style.color = "#0c5460";
			statusDiv.setText("ℹ Enter an API key to connect");
		}

		new Setting(containerEl)
			.setName("Test Connection")
			.setDesc("Send a test webhook to verify your setup")
			.addButton((button) => {
				button
					.setButtonText("Send Test")
					.onClick(async () => {
						if (!this.plugin.settings.clientKey) {
							new Notice("Please enter a client key first");
							return;
						}
						
						// Test SSE connection instead of webhook (since client key is read-only)
						try {
							const testUrl = `${this.plugin.settings.endpoint}/events/${this.plugin.settings.clientKey}`;
							const response = await fetch(testUrl, {
								method: "GET",
							});
							
							if (response.ok) {
								new Notice("✅ SSE connection test successful!");
							} else {
								new Notice(`❌ SSE test failed: ${response.status} ${response.statusText}`);
							}
						} catch (err) {
							new Notice(`❌ SSE test failed: ${err.message}`);
						}
					});
			});
	}
}