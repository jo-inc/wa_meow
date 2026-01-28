package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type App struct {
	client       *whatsmeow.Client
	currentChat  types.JID
	chats        []ChatInfo
	messageStore sync.Map // map[string][]StoredMessage - messages by chat JID
}

type StoredMessage struct {
	ID        string
	Sender    string
	Text      string
	Timestamp time.Time
	IsFromMe  bool
}

type ChatInfo struct {
	JID          types.JID
	Name         string
	LastActivity time.Time
}

func main() {
	ctx := context.Background()
	dbLog := waLog.Stdout("Database", "ERROR", true)
	container, err := sqlstore.New(ctx, "sqlite3", "file:whatsapp.db?_foreign_keys=on", dbLog)
	if err != nil {
		panic(err)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		panic(err)
	}

	clientLog := waLog.Stdout("Client", "ERROR", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)

	app := &App{client: client}

	client.AddEventHandler(app.eventHandler)

	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			panic(err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\n📱 Scan this QR code with WhatsApp:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else {
				fmt.Println("Login event:", evt.Event)
			}
		}
	} else {
		err = client.Connect()
		if err != nil {
			panic(err)
		}
	}

	fmt.Println("\n✅ Connected to WhatsApp!")
	fmt.Println("Type 'help' for available commands.\n")

	go app.runREPL()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	client.Disconnect()
}

func (a *App) eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		sender := v.Info.Sender.User
		if v.Info.PushName != "" {
			sender = v.Info.PushName
		}

		text := ""
		if v.Message.Conversation != nil {
			text = *v.Message.Conversation
		} else if v.Message.ExtendedTextMessage != nil {
			text = *v.Message.ExtendedTextMessage.Text
		}

		if text != "" {
			// Store the message
			a.storeMessage(v.Info.Chat.String(), StoredMessage{
				ID:        v.Info.ID,
				Sender:    sender,
				Text:      text,
				Timestamp: v.Info.Timestamp,
				IsFromMe:  v.Info.IsFromMe,
			})

			// Display if in current chat
			if v.Info.Chat == a.currentChat {
				direction := "📨"
				if v.Info.IsFromMe {
					direction = "📤"
				}
				fmt.Printf("\n%s [%s] %s: %s\n> ", direction, v.Info.Timestamp.Format("15:04"), sender, text)
			}
		}

	case *events.HistorySync:
		a.handleHistorySync(v)

	case *events.OfflineSyncPreview:
		fmt.Printf("\n📥 Syncing %d missed messages...\n> ", v.Messages)

	case *events.OfflineSyncCompleted:
		fmt.Printf("\n✅ Offline sync complete (%d messages)\n> ", v.Count)
	}
}

func (a *App) storeMessage(chatJID string, msg StoredMessage) {
	existing, _ := a.messageStore.Load(chatJID)
	var messages []StoredMessage
	if existing != nil {
		messages = existing.([]StoredMessage)
	}
	messages = append(messages, msg)
	a.messageStore.Store(chatJID, messages)
}

func (a *App) handleHistorySync(evt *events.HistorySync) {
	syncType := evt.Data.GetSyncType().String()
	conversations := evt.Data.GetConversations()

	fmt.Printf("\n📜 History sync (%s): %d chats\n", syncType, len(conversations))

	totalMessages := 0
	for _, conv := range conversations {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}

		for _, historyMsg := range conv.GetMessages() {
			msg := a.parseHistoryMessage(chatJID, historyMsg.GetMessage())
			if msg != nil {
				a.storeMessage(chatJID.String(), *msg)
				totalMessages++
			}
		}
	}

	fmt.Printf("📜 Stored %d messages from history\n> ", totalMessages)
}

func (a *App) parseHistoryMessage(chatJID types.JID, webMsg *waWeb.WebMessageInfo) *StoredMessage {
	if webMsg == nil {
		return nil
	}

	parsedEvt, err := a.client.ParseWebMessage(chatJID, webMsg)
	if err != nil {
		return nil
	}

	text := ""
	if parsedEvt.Message.Conversation != nil {
		text = *parsedEvt.Message.Conversation
	} else if parsedEvt.Message.ExtendedTextMessage != nil && parsedEvt.Message.ExtendedTextMessage.Text != nil {
		text = *parsedEvt.Message.ExtendedTextMessage.Text
	}

	if text == "" {
		return nil
	}

	sender := parsedEvt.Info.Sender.User
	if parsedEvt.Info.PushName != "" {
		sender = parsedEvt.Info.PushName
	}

	return &StoredMessage{
		ID:        parsedEvt.Info.ID,
		Sender:    sender,
		Text:      text,
		Timestamp: parsedEvt.Info.Timestamp,
		IsFromMe:  parsedEvt.Info.IsFromMe,
	}
}

func (a *App) runREPL() {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}

		parts := strings.SplitN(input, " ", 2)
		cmd := strings.ToLower(parts[0])
		args := ""
		if len(parts) > 1 {
			args = parts[1]
		}

		switch cmd {
		case "help":
			a.showHelp()
		case "chats", "list":
			a.listChats()
		case "search":
			a.searchChats(args)
		case "open":
			a.openChat(args)
		case "messages", "msgs":
			a.showMessages()
		case "send":
			a.sendMessage(args)
		case "status":
			a.showStatus()
		case "quit", "exit":
			fmt.Println("Goodbye!")
			os.Exit(0)
		default:
			fmt.Println("Unknown command. Type 'help' for available commands.")
		}
	}
}

func (a *App) showHelp() {
	fmt.Println(`
📱 WhatsApp CLI Commands:
  chats / list      - List all chats
  search <query>    - Search chats by name
  open <number>     - Open chat by number from list
  messages / msgs   - Show messages in current chat
  send <message>    - Send message to current chat
  status            - Show connection status
  quit / exit       - Exit the program
`)
}

func (a *App) listChats() {
	fmt.Println("\n📋 Loading chats...")
	ctx := context.Background()
	
	groups, err := a.client.GetJoinedGroups(ctx)
	if err != nil {
		fmt.Printf("Error getting groups: %v\n", err)
	}

	a.chats = []ChatInfo{}

	for _, group := range groups {
		a.chats = append(a.chats, ChatInfo{
			JID:          group.JID,
			Name:         group.Name,
			LastActivity: time.Now(),
		})
	}

	contacts, err := a.client.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		fmt.Printf("Error getting contacts: %v\n", err)
	}

	for jid, contact := range contacts {
		name := contact.PushName
		if name == "" {
			name = contact.FullName
		}
		if name == "" {
			name = jid.User
		}
		a.chats = append(a.chats, ChatInfo{
			JID:  jid,
			Name: name,
		})
	}

	sort.Slice(a.chats, func(i, j int) bool {
		return a.chats[i].Name < a.chats[j].Name
	})

	fmt.Printf("\n📱 Found %d chats:\n", len(a.chats))
	for i, chat := range a.chats {
		chatType := "👤"
		if chat.JID.Server == types.GroupServer {
			chatType = "👥"
		}
		fmt.Printf("  %3d. %s %s (%s)\n", i+1, chatType, chat.Name, chat.JID.User)
	}
	fmt.Println()
}

func (a *App) searchChats(query string) {
	if query == "" {
		fmt.Println("Usage: search <query>")
		return
	}

	if len(a.chats) == 0 {
		fmt.Println("No chats loaded. Run 'chats' first.")
		return
	}

	query = strings.ToLower(query)
	fmt.Printf("\n🔍 Searching for '%s':\n", query)
	
	found := 0
	for i, chat := range a.chats {
		if strings.Contains(strings.ToLower(chat.Name), query) ||
			strings.Contains(chat.JID.User, query) {
			chatType := "👤"
			if chat.JID.Server == types.GroupServer {
				chatType = "👥"
			}
			fmt.Printf("  %3d. %s %s (%s)\n", i+1, chatType, chat.Name, chat.JID.User)
			found++
		}
	}
	
	if found == 0 {
		fmt.Println("  No matches found.")
	}
	fmt.Println()
}

func (a *App) openChat(args string) {
	if args == "" {
		fmt.Println("Usage: open <number>")
		return
	}

	if len(a.chats) == 0 {
		fmt.Println("No chats loaded. Run 'chats' first.")
		return
	}

	num, err := strconv.Atoi(args)
	if err != nil || num < 1 || num > len(a.chats) {
		fmt.Printf("Invalid chat number. Use 1-%d\n", len(a.chats))
		return
	}

	chat := a.chats[num-1]
	a.currentChat = chat.JID
	fmt.Printf("\n✅ Opened chat: %s\n", chat.Name)

	// Show synced messages for this chat
	a.showMessages()
}

func (a *App) showMessages() {
	if a.currentChat.IsEmpty() {
		fmt.Println("No chat open. Use 'open <number>' first.")
		return
	}

	chatJID := a.currentChat.String()
	stored, ok := a.messageStore.Load(chatJID)
	if !ok || stored == nil {
		fmt.Println("\n💬 No messages synced for this chat yet.")
		fmt.Println("Messages will appear as they arrive.\n")
		return
	}

	messages := stored.([]StoredMessage)
	if len(messages) == 0 {
		fmt.Println("\n💬 No messages synced for this chat yet.")
		fmt.Println("Messages will appear as they arrive.\n")
		return
	}

	// Sort by timestamp
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Timestamp.Before(messages[j].Timestamp)
	})

	// Show last 20 messages
	start := 0
	if len(messages) > 20 {
		start = len(messages) - 20
		fmt.Printf("\n💬 Last 20 of %d messages:\n", len(messages))
	} else {
		fmt.Printf("\n💬 %d messages:\n", len(messages))
	}

	fmt.Println("────────────────────────────────────────")
	for _, msg := range messages[start:] {
		direction := "←"
		if msg.IsFromMe {
			direction = "→"
		}
		// Truncate long messages
		text := msg.Text
		if len(text) > 80 {
			text = text[:77] + "..."
		}
		fmt.Printf("%s [%s] %s: %s\n", direction, msg.Timestamp.Format("Jan 02 15:04"), msg.Sender, text)
	}
	fmt.Println("────────────────────────────────────────")
	fmt.Println()
}

func (a *App) sendMessage(text string) {
	if text == "" {
		fmt.Println("Usage: send <message>")
		return
	}

	if a.currentChat.IsEmpty() {
		fmt.Println("No chat open. Use 'open <number>' first.")
		return
	}

	msg := &waE2E.Message{
		Conversation: proto.String(text),
	}

	resp, err := a.client.SendMessage(context.Background(), a.currentChat, msg)
	if err != nil {
		fmt.Printf("❌ Error sending message: %v\n", err)
		return
	}

	fmt.Printf("✅ Message sent! (ID: %s, Timestamp: %s)\n", resp.ID, resp.Timestamp.Format("15:04:05"))
}

func (a *App) showStatus() {
	connected := a.client.IsConnected()
	loggedIn := a.client.IsLoggedIn()
	
	fmt.Printf("\n📊 Status:\n")
	fmt.Printf("  Connected: %v\n", connected)
	fmt.Printf("  Logged In: %v\n", loggedIn)
	
	if a.client.Store.ID != nil {
		fmt.Printf("  Phone: %s\n", a.client.Store.ID.User)
	}
	
	if !a.currentChat.IsEmpty() {
		for _, chat := range a.chats {
			if chat.JID == a.currentChat {
				fmt.Printf("  Current Chat: %s\n", chat.Name)
				break
			}
		}
	}
	fmt.Println()
}
