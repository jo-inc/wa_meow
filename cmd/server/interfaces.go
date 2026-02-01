package main

import (
	"context"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// WhatsAppClient abstracts the whatsmeow.Client for testing
type WhatsAppClient interface {
	// Connection state
	IsConnected() bool
	IsLoggedIn() bool
	Connect() error
	Disconnect()

	// QR login
	GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)

	// Messaging
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	SendChatPresence(ctx context.Context, jid types.JID, presence types.ChatPresence, media types.ChatPresenceMedia) error

	// Media
	Upload(ctx context.Context, plaintext []byte, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error)

	// Groups
	GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error)
	GetGroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error)

	// Store access
	GetStore() DeviceStore

	// Event handling
	AddEventHandler(handler whatsmeow.EventHandler) uint32
}

// DeviceStore abstracts access to device/store information
type DeviceStore interface {
	GetID() *types.JID
	GetContacts() ContactStore
}

// ContactStore abstracts access to contacts
type ContactStore interface {
	GetAllContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error)
}

// realClientWrapper wraps the real whatsmeow.Client to implement WhatsAppClient
type realClientWrapper struct {
	client *whatsmeow.Client
}

func newRealClientWrapper(client *whatsmeow.Client) *realClientWrapper {
	return &realClientWrapper{client: client}
}

func (w *realClientWrapper) IsConnected() bool {
	return w.client.IsConnected()
}

func (w *realClientWrapper) IsLoggedIn() bool {
	return w.client.IsLoggedIn()
}

func (w *realClientWrapper) Connect() error {
	return w.client.Connect()
}

func (w *realClientWrapper) Disconnect() {
	w.client.Disconnect()
}

func (w *realClientWrapper) GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	return w.client.GetQRChannel(ctx)
}

func (w *realClientWrapper) SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	return w.client.SendMessage(ctx, to, message, extra...)
}

func (w *realClientWrapper) SendChatPresence(ctx context.Context, jid types.JID, presence types.ChatPresence, media types.ChatPresenceMedia) error {
	return w.client.SendChatPresence(ctx, jid, presence, media)
}

func (w *realClientWrapper) GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error) {
	return w.client.GetJoinedGroups(ctx)
}

func (w *realClientWrapper) GetGroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error) {
	return w.client.GetGroupInfo(ctx, jid)
}

func (w *realClientWrapper) Upload(ctx context.Context, plaintext []byte, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	return w.client.Upload(ctx, plaintext, appInfo)
}

func (w *realClientWrapper) Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error) {
	return w.client.Download(ctx, msg)
}

func (w *realClientWrapper) AddEventHandler(handler whatsmeow.EventHandler) uint32 {
	return w.client.AddEventHandler(handler)
}

func (w *realClientWrapper) GetStore() DeviceStore {
	return &realDeviceStoreWrapper{w.client.Store}
}

// realDeviceStoreWrapper wraps the real store.Device to implement DeviceStore
type realDeviceStoreWrapper struct {
	store *store.Device
}

func (w *realDeviceStoreWrapper) GetID() *types.JID {
	return w.store.ID
}

func (w *realDeviceStoreWrapper) GetContacts() ContactStore {
	return w.store.Contacts
}
