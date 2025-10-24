package device

import (
	"sync"
)

// Manager 管理所有設備和客戶端會話
type Manager struct {
	clientsMu sync.RWMutex
	clients   map[string]*ClientSession // clientID -> session

	devicesMu      sync.RWMutex
	deviceSessions map[string]*DeviceSession // deviceIP -> device session
}

// NewManager 創建新的設備管理器
func NewManager() *Manager {
	return &Manager{
		clients:        make(map[string]*ClientSession),
		deviceSessions: make(map[string]*DeviceSession),
	}
}

// AddClient 添加客戶端會話
func (m *Manager) AddClient(sessionID string, session *ClientSession) {
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()
	m.clients[sessionID] = session
}

// RemoveClient 移除客戶端會話
func (m *Manager) RemoveClient(sessionID string) {
	m.clientsMu.Lock()
	defer m.clientsMu.Unlock()
	delete(m.clients, sessionID)
}

// GetClient 獲取客戶端會話
func (m *Manager) GetClient(sessionID string) (*ClientSession, bool) {
	m.clientsMu.RLock()
	defer m.clientsMu.RUnlock()
	session, ok := m.clients[sessionID]
	return session, ok
}

// GetClientsByDevice 獲取連接到指定設備的所有客戶端
func (m *Manager) GetClientsByDevice(deviceIP string) []*ClientSession {
	m.clientsMu.RLock()
	defer m.clientsMu.RUnlock()

	var result []*ClientSession
	for _, session := range m.clients {
		if session.DeviceIP == deviceIP {
			result = append(result, session)
		}
	}
	return result
}

// AddDevice 添加設備會話
func (m *Manager) AddDevice(deviceIP string, session *DeviceSession) {
	m.devicesMu.Lock()
	defer m.devicesMu.Unlock()
	m.deviceSessions[deviceIP] = session
}

// RemoveDevice 移除設備會話
func (m *Manager) RemoveDevice(deviceIP string) {
	m.devicesMu.Lock()
	defer m.devicesMu.Unlock()
	delete(m.deviceSessions, deviceIP)
}

// GetDevice 獲取設備會話
func (m *Manager) GetDevice(deviceIP string) (*DeviceSession, bool) {
	m.devicesMu.RLock()
	defer m.devicesMu.RUnlock()
	session, ok := m.deviceSessions[deviceIP]
	return session, ok
}

// ListDevices 列出所有設備 IP
func (m *Manager) ListDevices() []string {
	m.devicesMu.RLock()
	defer m.devicesMu.RUnlock()

	devices := make([]string, 0, len(m.deviceSessions))
	for ip := range m.deviceSessions {
		devices = append(devices, ip)
	}
	return devices
}
