package machine

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/datastore"
)

// journal.go is the undo journal (apis.mdx §9.6). ezBookkeeping has no undo stack, so every
// machine-plane write records its own inverse here, and /undo applies it.

// MachineJournal is one machine-plane write. Table owned by pkg/machine (prefix machine_).
type MachineJournal struct {
	JournalId       int64  `xorm:"PK AUTOINCR"`
	Uid             int64  `xorm:"INDEX(IDX_machine_journal_uid_undone) NOT NULL"`
	Undone          bool   `xorm:"INDEX(IDX_machine_journal_uid_undone) NOT NULL"`
	Route           string `xorm:"VARCHAR(128) NOT NULL"`
	Client          string `xorm:"VARCHAR(64)"`
	Summary         string `xorm:"VARCHAR(255)"`
	Changes         int    `xorm:"NOT NULL"`
	Inverse         string `xorm:"TEXT"`
	CreatedUnixTime int64  `xorm:"NOT NULL"`
	UndoneUnixTime  int64
}

// InverseOp is one step of undoing a write. Kind names a registered executor; Payload is whatever
// that executor needs (ids, prior field values). Check, when set, is what the rows must still look
// like for the undo to be safe — if the operator edited a row since, undo refuses (§9.6).
type InverseOp struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
	Check   json.RawMessage `json:"check,omitempty"`
	// Redo is the forward op that re-applies this step after an undo
	Redo *InverseOp `json:"redo,omitempty"`
}

// InverseExecutor applies one inverse op. It must refuse with a conflict Fail when check says the
// rows moved since the write.
type InverseExecutor func(mc *Ctx, payload json.RawMessage, check json.RawMessage) error

var inverseRegistry = struct {
	sync.RWMutex
	m map[string]InverseExecutor
}{m: map[string]InverseExecutor{}}

// RegisterInverse registers an executor for an inverse op kind (call from a family's init())
func RegisterInverse(kind string, fn InverseExecutor) {
	inverseRegistry.Lock()
	defer inverseRegistry.Unlock()

	inverseRegistry.m[kind] = fn
}

// LookupInverse returns the executor for a kind
func LookupInverse(kind string) (InverseExecutor, bool) {
	inverseRegistry.RLock()
	defer inverseRegistry.RUnlock()

	fn, ok := inverseRegistry.m[kind]

	return fn, ok
}

// NewInverseOp builds an op from Go values
func NewInverseOp(kind string, payload any, check any) InverseOp {
	p, _ := json.Marshal(payload)
	op := InverseOp{Kind: kind, Payload: p}

	if check != nil {
		c, _ := json.Marshal(check)
		op.Check = c
	}

	return op
}

// RecordJournal appends a journal entry for a write just applied and returns its id. Ops are
// applied by undo in REVERSE order.
func RecordJournal(mc *Ctx, summary string, changes int, ops []InverseOp) (int64, error) {
	if len(ops) == 0 {
		return 0, nil
	}

	inverse, err := json.Marshal(ops)

	if err != nil {
		return 0, err
	}

	client := mc.Client

	if len(client) > 64 {
		client = client[:64]
	}

	if len(summary) > 255 {
		summary = summary[:252] + "..."
	}

	entry := &MachineJournal{
		Uid:             mc.Uid,
		Route:           mc.Route.Method + " " + mc.Route.Path,
		Client:          client,
		Summary:         summary,
		Changes:         changes,
		Inverse:         string(inverse),
		CreatedUnixTime: time.Now().Unix(),
	}

	_, err = datastore.Container.UserDataStore.Choose(mc.Uid).NewSession(mc.Web).Insert(entry)

	if err != nil {
		return 0, fmt.Errorf("journal insert failed: %w", err)
	}

	mc.SetMeta("journalId", entry.JournalId)

	return entry.JournalId, nil
}

// machineTables are the tables pkg/machine owns; Arm() syncs them. Families add theirs from init().
var machineTables = []any{new(MachineJournal)}

func registerTable(bean any) {
	machineTables = append(machineTables, bean)
}
