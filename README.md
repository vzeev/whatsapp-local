# WhatsApp Local

Collect text from selected WhatsApp groups into a local SQLite file, then read or summarize it with Hermes or another assistant that can run shell commands. The collector uses the same library as the Hermes integration it was adapted from. It runs from one shell script and works without MCP, Docker, or a model API key. [whatsmeow](https://github.com/tulir/whatsmeow)

## Install with a local Hermes agent

Give Hermes the Git repository URL and this prompt.

> Install WhatsApp Local from `<repository URL>`. Read its README and `.hermes/skills/whatsapp-local/SKILL.md`. Set it up on this computer and guide me through pairing in my private terminal. Then help me choose a WhatsApp group and summarize a live message from it.

Hermes uses its local terminal, clones the repo into `~/.local/share/whatsapp-local` by default, and can install the bundled skill into the current profile for future chats. You scan the QR code with the WhatsApp app on your phone. [Hermes setup and usage skill](.hermes/skills/whatsapp-local/SKILL.md)

Once collection is running, ask Hermes to summarize a confirmed group ID and time window, such as the last 24 hours.

## Requirements

- **Mac.** Install Go 1.26.9 or newer and Apple’s Command Line Tools with `xcode-select --install`. [Go downloads](https://go.dev/dl/), [Apple Command Line Tools](https://developer.apple.com/documentation/xcode/installing-the-command-line-tools)
- **Linux.** Install Go 1.26.9 or newer and a C compiler. On Ubuntu or Debian, install the compiler with `sudo apt install build-essential`. [Go installation](https://go.dev/doc/install)
- **Windows.** Run the Linux setup inside Ubuntu on WSL 2, with the checkout in Ubuntu’s home directory. The current script requires a Unix environment. [Microsoft WSL setup](https://learn.microsoft.com/windows/wsl/install)

You also need Git to clone the repo and internet access for the first build. SQLite is bundled.

## Connect your own group

1. From the Git checkout, run `./wa pair` in a private terminal. The first run downloads dependencies and builds the tool. On your phone, open WhatsApp → Linked devices → Link a device and scan the QR code.
2. Run `./wa groups`. Copy the `group_id` of a test group you have permission to collect from. Community announcement subgroups appear as their own groups.
3. Start the collector with the group ID from that output. Replace `GROUP_ID` with the actual ID in both commands below. Pass more IDs to watch several groups. Once the collector reports that it is connected, send a test message to the group.

   ```sh
   ./wa watch GROUP_ID
   ```

4. With the collector still running, read the group from a second terminal in the same checkout.

   ```sh
   ./wa read GROUP_ID -since 24h -limit 200
   ```

To summarize, give the `./wa read` command to an assistant with terminal access to the same checkout. Ask it to include the collection start, any row limit, and message IDs for key points. If Hermes runs in a container, run the command on the host and give Hermes the output. A remote model receives the message text you choose to send it.

## What it collects

- A group’s collection starts the first time you run `watch` with its ID. The script saves newly delivered text and supported attachment captions and indicators. It skips historical sync, direct chats, disappearing and view-once messages, and edits. It reads from local collection time onward, independent of your phone's unread status. Gaps are possible while the collector is offline.
- `read` queries one group locally, even after the collector stops. It prints the selected rows oldest first. The default window is 24 hours with a limit of 200 rows. The script reports when that limit truncates the window. It never sends messages, marks them read, or downloads attachment files.
- `.whatsapp/messages.sqlite3` contains chat text. `.whatsapp/session.sqlite3` contains linked-device credentials and protocol state. Git ignores the entire `.whatsapp/` directory. Keep it private and share from a test group on a conference screen. To disconnect the integration, remove the linked device from your phone.
- The collector uses an unofficial client. WhatsApp warns that linking through unofficial clients can put an account at risk. [WhatsApp guidance on linked devices](https://faq.whatsapp.com/378279804439436/)
