package main

import (
	"context"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// MockWhatsAppClient implements WhatsAppClient for testing
type MockWhatsAppClient struct {
	mu sync.Mutex

	// State
	connected bool
	loggedIn  bool

	// Configurable return values
	ConnectError        error
	SendMessageResponse whatsmeow.SendResponse
	SendMessageError    error
	SendPresenceError   error
	UploadResponse      whatsmeow.UploadResponse
	UploadError         error
	DownloadData        []byte
	DownloadError       error
	JoinedGroups        []*types.GroupInfo
	JoinedGroupsError   error
	GroupInfo           *types.GroupInfo
	GroupInfoError      error
	QRChannelError      error

	// Store mock
	store *MockDeviceStore

	// Call tracking
	Calls []MockCall
}

// MockCall records a method invocation
type MockCall struct {
	Method    string
	Args      []interface{}
	Timestamp time.Time
}

// MockDeviceStore implements DeviceStore for testing
type MockDeviceStore struct {
	ID       *types.JID
	Contacts *MockContactStore
}

func (s *MockDeviceStore) GetID() *types.JID {
	return s.ID
}

func (s *MockDeviceStore) GetContacts() ContactStore {
	return s.Contacts
}

// MockContactStore implements ContactStore for testing
type MockContactStore struct {
	AllContacts   map[types.JID]types.ContactInfo
	ContactsError error
}

func (c *MockContactStore) GetAllContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
	return c.AllContacts, c.ContactsError
}

// NewMockClient creates a disconnected mock client
func NewMockClient() *MockWhatsAppClient {
	return &MockWhatsAppClient{
		connected: false,
		loggedIn:  false,
		store: &MockDeviceStore{
			ID:       nil,
			Contacts: &MockContactStore{AllContacts: make(map[types.JID]types.ContactInfo)},
		},
		Calls: make([]MockCall, 0),
	}
}

// NewConnectedMockClient creates a connected but not logged in mock client
func NewConnectedMockClient() *MockWhatsAppClient {
	m := NewMockClient()
	m.connected = true
	return m
}

// NewLoggedInMockClient creates a fully connected and logged in mock client
func NewLoggedInMockClient() *MockWhatsAppClient {
	m := NewMockClient()
	m.connected = true
	m.loggedIn = true
	m.store.ID = &types.JID{User: "1234567890", Server: types.DefaultUserServer}
	return m
}

func (m *MockWhatsAppClient) recordCall(method string, args ...interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls = append(m.Calls, MockCall{
		Method:    method,
		Args:      args,
		Timestamp: time.Now(),
	})
}

// GetCalls returns all recorded calls (thread-safe)
func (m *MockWhatsAppClient) GetCalls() []MockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	calls := make([]MockCall, len(m.Calls))
	copy(calls, m.Calls)
	return calls
}

// GetCallsByMethod returns calls for a specific method
func (m *MockWhatsAppClient) GetCallsByMethod(method string) []MockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []MockCall
	for _, c := range m.Calls {
		if c.Method == method {
			result = append(result, c)
		}
	}
	return result
}

// SetConnected sets the connected state
func (m *MockWhatsAppClient) SetConnected(connected bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = connected
}

// SetLoggedIn sets the logged in state
func (m *MockWhatsAppClient) SetLoggedIn(loggedIn bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loggedIn = loggedIn
}

// SetDeviceID sets the device ID for Store.ID access
func (m *MockWhatsAppClient) SetDeviceID(jid *types.JID) {
	m.store.ID = jid
}

// SetContacts sets the contacts for Store.Contacts access
func (m *MockWhatsAppClient) SetContacts(contacts map[types.JID]types.ContactInfo) {
	m.store.Contacts.AllContacts = contacts
}

// WhatsAppClient interface implementation

func (m *MockWhatsAppClient) IsConnected() bool {
	m.recordCall("IsConnected")
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

func (m *MockWhatsAppClient) IsLoggedIn() bool {
	m.recordCall("IsLoggedIn")
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loggedIn
}

func (m *MockWhatsAppClient) Connect() error {
	m.recordCall("Connect")
	if m.ConnectError != nil {
		return m.ConnectError
	}
	m.mu.Lock()
	m.connected = true
	m.mu.Unlock()
	return nil
}

func (m *MockWhatsAppClient) Disconnect() {
	m.recordCall("Disconnect")
	m.mu.Lock()
	m.connected = false
	m.mu.Unlock()
}

func (m *MockWhatsAppClient) GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	m.recordCall("GetQRChannel", ctx)
	if m.QRChannelError != nil {
		return nil, m.QRChannelError
	}
	ch := make(chan whatsmeow.QRChannelItem, 10)
	return ch, nil
}

func (m *MockWhatsAppClient) SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	m.recordCall("SendMessage", ctx, to, message, extra)
	if m.SendMessageError != nil {
		return whatsmeow.SendResponse{}, m.SendMessageError
	}
	if m.SendMessageResponse.ID == "" {
		return whatsmeow.SendResponse{
			ID:        "mock-msg-id",
			Timestamp: time.Now(),
		}, nil
	}
	return m.SendMessageResponse, nil
}

func (m *MockWhatsAppClient) SendChatPresence(ctx context.Context, jid types.JID, presence types.ChatPresence, media types.ChatPresenceMedia) error {
	m.recordCall("SendChatPresence", ctx, jid, presence, media)
	return m.SendPresenceError
}

func (m *MockWhatsAppClient) Upload(ctx context.Context, plaintext []byte, appInfo whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	m.recordCall("Upload", ctx, plaintext, appInfo)
	if m.UploadError != nil {
		return whatsmeow.UploadResponse{}, m.UploadError
	}
	if m.UploadResponse.URL == "" {
		return whatsmeow.UploadResponse{
			URL:           "https://mock.whatsapp.net/media/123",
			DirectPath:    "/v/mock/123",
			MediaKey:      []byte("mock-media-key"),
			FileEncSHA256: []byte("mock-enc-sha"),
			FileSHA256:    []byte("mock-sha"),
		}, nil
	}
	return m.UploadResponse, nil
}

func (m *MockWhatsAppClient) Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error) {
	m.recordCall("Download", ctx, msg)
	if m.DownloadError != nil {
		return nil, m.DownloadError
	}
	if m.DownloadData == nil {
		return []byte("mock-image-data"), nil
	}
	return m.DownloadData, nil
}

func (m *MockWhatsAppClient) GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error) {
	m.recordCall("GetJoinedGroups", ctx)
	return m.JoinedGroups, m.JoinedGroupsError
}

func (m *MockWhatsAppClient) GetGroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error) {
	m.recordCall("GetGroupInfo", ctx, jid)
	return m.GroupInfo, m.GroupInfoError
}

func (m *MockWhatsAppClient) GetStore() DeviceStore {
	m.recordCall("GetStore")
	return m.store
}

func (m *MockWhatsAppClient) AddEventHandler(handler whatsmeow.EventHandler) uint32 {
	m.recordCall("AddEventHandler", handler)
	return 0
}
