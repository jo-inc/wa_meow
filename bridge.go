package main

/*
#include <stdlib.h>
*/
import "C"
import (
	"context"
	"encoding/json"
	"sync"
	"time"
	"unsafe"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var (
	client         *whatsmeow.Client
	container      *sqlstore.Container
	eventCallback  func(string)
	mu             sync.Mutex
	qrCodeChannel  chan string
	loginDone      chan bool
)

type ChatJSON struct {
	JID      string `json:"jid"`
	Name     string `json:"name"`
	IsGroup  bool   `json:"is_group"`
	LastSeen int64  `json:"last_seen,omitempty"`
}

type MessageJSON struct {
	ID        string `json:"id"`
	ChatJID   string `json:"chat_jid"`
	SenderJID string `json:"sender_jid"`
	SenderName string `json:"sender_name"`
	Text      string `json:"text"`
	Timestamp int64  `json:"timestamp"`
	IsFromMe  bool   `json:"is_from_me"`
}

type EventJSON struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

//export WhatsAppInit
func WhatsAppInit(dbPath *C.char) *C.char {
	mu.Lock()
	defer mu.Unlock()

	ctx := context.Background()
	dbPathGo := C.GoString(dbPath)
	
	dbLog := waLog.Stdout("Database", "ERROR", true)
	var err error
	container, err = sqlstore.New(ctx, "sqlite3", "file:"+dbPathGo+"?_foreign_keys=on", dbLog)
	if err != nil {
		return C.CString(`{"error":"` + err.Error() + `"}`)
	}

	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		return C.CString(`{"error":"` + err.Error() + `"}`)
	}

	clientLog := waLog.Stdout("Client", "ERROR", true)
	client = whatsmeow.NewClient(deviceStore, clientLog)
	client.AddEventHandler(handleEvent)

	qrCodeChannel = make(chan string, 10)
	loginDone = make(chan bool, 1)

	return C.CString(`{"status":"initialized"}`)
}

//export WhatsAppConnect
func WhatsAppConnect() *C.char {
	mu.Lock()
	defer mu.Unlock()

	if client == nil {
		return C.CString(`{"error":"not initialized"}`)
	}

	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(context.Background())
		err := client.Connect()
		if err != nil {
			return C.CString(`{"error":"` + err.Error() + `"}`)
		}

		go func() {
			for evt := range qrChan {
				if evt.Event == "code" {
					qrCodeChannel <- evt.Code
				} else if evt.Event == "success" {
					loginDone <- true
					return
				}
			}
		}()

		return C.CString(`{"status":"needs_qr"}`)
	}

	err := client.Connect()
	if err != nil {
		return C.CString(`{"error":"` + err.Error() + `"}`)
	}

	return C.CString(`{"status":"connected"}`)
}

//export WhatsAppGetQRCode
func WhatsAppGetQRCode(timeoutMs C.int) *C.char {
	select {
	case code := <-qrCodeChannel:
		return C.CString(`{"qr_code":"` + code + `"}`)
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return C.CString(`{"status":"timeout"}`)
	case <-loginDone:
		return C.CString(`{"status":"logged_in"}`)
	}
}

//export WhatsAppIsConnected
func WhatsAppIsConnected() C.int {
	if client != nil && client.IsConnected() {
		return 1
	}
	return 0
}

//export WhatsAppIsLoggedIn
func WhatsAppIsLoggedIn() C.int {
	if client != nil && client.IsLoggedIn() {
		return 1
	}
	return 0
}

//export WhatsAppGetChats
func WhatsAppGetChats() *C.char {
	if client == nil {
		return C.CString(`{"error":"not initialized"}`)
	}

	ctx := context.Background()
	var chats []ChatJSON

	groups, err := client.GetJoinedGroups(ctx)
	if err == nil {
		for _, group := range groups {
			chats = append(chats, ChatJSON{
				JID:     group.JID.String(),
				Name:    group.Name,
				IsGroup: true,
			})
		}
	}

	contacts, err := client.Store.Contacts.GetAllContacts(ctx)
	if err == nil {
		for jid, contact := range contacts {
			name := contact.PushName
			if name == "" {
				name = contact.FullName
			}
			if name == "" {
				name = jid.User
			}
			chats = append(chats, ChatJSON{
				JID:     jid.String(),
				Name:    name,
				IsGroup: false,
			})
		}
	}

	jsonData, _ := json.Marshal(chats)
	return C.CString(string(jsonData))
}

//export WhatsAppSendMessage
func WhatsAppSendMessage(jidStr *C.char, text *C.char) *C.char {
	if client == nil {
		return C.CString(`{"error":"not initialized"}`)
	}

	jid, err := types.ParseJID(C.GoString(jidStr))
	if err != nil {
		return C.CString(`{"error":"invalid jid: ` + err.Error() + `"}`)
	}

	msg := &waE2E.Message{
		Conversation: proto.String(C.GoString(text)),
	}

	resp, err := client.SendMessage(context.Background(), jid, msg)
	if err != nil {
		return C.CString(`{"error":"` + err.Error() + `"}`)
	}

	result := map[string]interface{}{
		"id":        resp.ID,
		"timestamp": resp.Timestamp.Unix(),
	}
	jsonData, _ := json.Marshal(result)
	return C.CString(string(jsonData))
}

//export WhatsAppDisconnect
func WhatsAppDisconnect() {
	mu.Lock()
	defer mu.Unlock()

	if client != nil {
		client.Disconnect()
	}
}

//export WhatsAppFreeString
func WhatsAppFreeString(str *C.char) {
	C.free(unsafe.Pointer(str))
}

var messageCallback unsafe.Pointer

//export WhatsAppSetMessageCallback
func WhatsAppSetMessageCallback(callback unsafe.Pointer) {
	messageCallback = callback
}

func main() {}

func handleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		text := ""
		if v.Message.Conversation != nil {
			text = *v.Message.Conversation
		} else if v.Message.ExtendedTextMessage != nil && v.Message.ExtendedTextMessage.Text != nil {
			text = *v.Message.ExtendedTextMessage.Text
		}

		msg := MessageJSON{
			ID:         v.Info.ID,
			ChatJID:    v.Info.Chat.String(),
			SenderJID:  v.Info.Sender.String(),
			SenderName: v.Info.PushName,
			Text:       text,
			Timestamp:  v.Info.Timestamp.Unix(),
			IsFromMe:   v.Info.IsFromMe,
		}

		jsonData, _ := json.Marshal(EventJSON{
			Type:    "message",
			Payload: msg,
		})

		if eventCallback != nil {
			eventCallback(string(jsonData))
		}
	}
}
