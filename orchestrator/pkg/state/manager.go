// Package state persists and recovers orchestrator state using Kubernetes ConfigMaps.
package state

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/wistefan/dev-env/orchestrator/pkg/assignment"
	"github.com/wistefan/dev-env/orchestrator/pkg/identity"
)

const (
	// ConfigMapName is the name of the ConfigMap used for state persistence.
	ConfigMapName = "orchestrator-state"

	// StateDataKey is the key within the ConfigMap that holds the JSON state.
	StateDataKey = "state.json"
)

// OrchestratorState is the complete persisted state of the orchestrator.
type OrchestratorState struct {
	Agents      []identity.AgentIdentity          `json:"agents"`
	Queue       []assignment.QueueEntry           `json:"queue"`
	Assignments map[int]*assignment.TicketAssignment `json:"assignments"`
	Escalations map[int]*assignment.EscalationEntry  `json:"escalations"`
	PRMappings  map[string]int                    `json:"prMappings,omitempty"` // "{owner}/{repo}#{pr}" → ticket ID
	LastSaved   time.Time                         `json:"lastSaved"`
}

// Manager handles state persistence to a Kubernetes ConfigMap.
type Manager struct {
	clientset kubernetes.Interface
	namespace string

	mu              sync.Mutex
	resourceVersion string // for optimistic locking
	debounceTimer   *time.Timer
	debounceDur     time.Duration
	pendingSave     bool
}

// NewManager creates a new state manager.
func NewManager(clientset kubernetes.Interface, namespace string) *Manager {
	return &Manager{
		clientset:   clientset,
		namespace:   namespace,
		debounceDur: 2 * time.Second,
	}
}

// Save persists the given state to the ConfigMap. Uses optimistic locking
// via ConfigMap resourceVersion to prevent concurrent overwrites.
func (m *Manager) Save(ctx context.Context, state *OrchestratorState) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	state.LastSaved = time.Now()

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            ConfigMapName,
			Namespace:       m.namespace,
			ResourceVersion: m.resourceVersion,
		},
		Data: map[string]string{
			StateDataKey: string(data),
		},
	}

	var saved *corev1.ConfigMap
	if m.resourceVersion == "" {
		// Try to create
		saved, err = m.clientset.CoreV1().ConfigMaps(m.namespace).Create(ctx, cm, metav1.CreateOptions{})
		if errors.IsAlreadyExists(err) {
			// Exists but we don't have the version — get it first
			existing, getErr := m.clientset.CoreV1().ConfigMaps(m.namespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
			if getErr != nil {
				return fmt.Errorf("getting existing configmap: %w", getErr)
			}
			cm.ResourceVersion = existing.ResourceVersion
			saved, err = m.clientset.CoreV1().ConfigMaps(m.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		}
	} else {
		saved, err = m.clientset.CoreV1().ConfigMaps(m.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	}

	if err != nil {
		return fmt.Errorf("saving state: %w", err)
	}

	m.resourceVersion = saved.ResourceVersion
	log.Printf("State saved (version: %s, %d bytes)", saved.ResourceVersion, len(data))
	return nil
}

// Load reads the state from the ConfigMap. Returns nil state if the ConfigMap doesn't exist.
func (m *Manager) Load(ctx context.Context) (*OrchestratorState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cm, err := m.clientset.CoreV1().ConfigMaps(m.namespace).Get(ctx, ConfigMapName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			log.Println("No existing state found, starting fresh")
			return nil, nil
		}
		return nil, fmt.Errorf("loading state configmap: %w", err)
	}

	m.resourceVersion = cm.ResourceVersion

	dataStr, ok := cm.Data[StateDataKey]
	if !ok {
		log.Println("State ConfigMap exists but has no state data")
		return nil, nil
	}

	var state OrchestratorState
	if err := json.Unmarshal([]byte(dataStr), &state); err != nil {
		return nil, fmt.Errorf("unmarshaling state: %w", err)
	}

	log.Printf("State loaded (version: %s, last saved: %s, agents: %d, queue: %d, assignments: %d)",
		cm.ResourceVersion, state.LastSaved.Format(time.RFC3339), len(state.Agents),
		len(state.Queue), len(state.Assignments))

	return &state, nil
}

// SaveDebounced schedules a save after the debounce duration.
// Multiple calls within the debounce window result in a single save.
// The provided function is called to build the current state snapshot.
func (m *Manager) SaveDebounced(ctx context.Context, buildState func() *OrchestratorState) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.debounceTimer != nil {
		m.debounceTimer.Stop()
	}

	m.debounceTimer = time.AfterFunc(m.debounceDur, func() {
		state := buildState()
		if err := m.Save(ctx, state); err != nil {
			log.Printf("ERROR: debounced save failed: %v", err)
		}
	})
}

// Delete removes the state ConfigMap. Used during teardown.
func (m *Manager) Delete(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	err := m.clientset.CoreV1().ConfigMaps(m.namespace).Delete(ctx, ConfigMapName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting state configmap: %w", err)
	}

	m.resourceVersion = ""
	return nil
}
