package machine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mayswind/ezbookkeeping/pkg/api"
	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/datastore"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/services"
	"github.com/mayswind/ezbookkeeping/pkg/settings"
)

// routes_reference.go — the reference families: categories (apis.mdx §10.4), tags and tag groups
// (§10.5), templates and scheduled transactions with /schedules/upcoming (§10.6) and saved insights
// (§10.8).
//
// It also holds the "ref kit" the account, currency and user families share: name resolution
// (exact first, then case-insensitive, ambiguity is an error carrying the candidates), display-order
// moves, hide/delete/undelete plumbing, and the inverse executors every one of these writes
// registers so /undo can reverse it (§9.6).
//
// Every write goes through RunWrite (resolve → preview + confirm token → re-resolve → apply) and the
// apply half calls the SAME upstream /api/v1 handler the browser calls, as the bound user (R1).

// ---------------------------------------------------------------------------------------------
// the ref kit — shared by routes_accounts.go, routes_currency.go and routes_user.go
// ---------------------------------------------------------------------------------------------

// refFlex accepts a JSON string or a JSON number and keeps its literal text (icons are numbers in
// the UI and strings on upstream's wire; a caller should not have to know which)
type refFlex string

func (f *refFlex) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))

	if s == "null" {
		*f = ""
		return nil
	}

	if strings.HasPrefix(s, "\"") {
		var v string

		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}

		*f = refFlex(strings.TrimSpace(v))
		return nil
	}

	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()

	var n json.Number

	if err := dec.Decode(&n); err != nil {
		return fmt.Errorf("expected a string or a number")
	}

	*f = refFlex(n.String())

	return nil
}

var refDigitsRe = regexp.MustCompile(`^[0-9]+$`)
var refColorRe = regexp.MustCompile(`^[0-9a-f]{6}$`)

// refDefaultColor and refDefaultIcon are the web UI's defaults (src/consts/color.ts, icon.ts)
const (
	refDefaultColor = "000000"
	refDefaultIcon  = int64(1)
)

// refIcon parses an icon id; empty means def
func refIcon(v refFlex, def int64) (int64, error) {
	s := strings.TrimSpace(string(v))

	if s == "" {
		return def, nil
	}

	n, err := strconv.ParseInt(s, 10, 64)

	if err != nil || n < 1 {
		return 0, Invalid("icon is the numeric id of one of the app's built-in icons (1 is the default)", "icon %q is not an icon id", s)
	}

	return n, nil
}

// refColor normalises a 6-hex-digit colour; empty means def
func refColor(v, def string) (string, error) {
	s := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(v), "#"))

	if s == "" {
		return def, nil
	}

	if !refColorRe.MatchString(s) {
		return "", Invalid("colors are 6 hex digits, e.g. \"1e88e5\"", "color %q is not a hex RGB color", v)
	}

	return s, nil
}

// refName validates an entity name the way upstream's binding does (notBlank, max=64 runes)
func refName(noun, v string) (string, error) {
	s := strings.TrimSpace(v)

	if s == "" {
		return "", Invalid("pass a non-empty name", "%s name is required", noun)
	}

	if n := len([]rune(s)); n > 64 {
		return "", Invalid("shorten the name to 64 characters", "%s name is %d characters; the limit is 64", noun, n)
	}

	return s, nil
}

// refComment validates a comment (max 255)
func refComment(v string) (string, error) {
	if n := len([]rune(v)); n > 255 {
		return "", Invalid("shorten the comment to 255 characters", "the comment is %d characters; the limit is 255", n)
	}

	return v, nil
}

// refMatchByName returns the items whose name equals name exactly; failing that, those equal to it
// ignoring case (apis.mdx §10 conventions)
func refMatchByName[T any](items []T, name string, nameOf func(T) string) []T {
	name = strings.TrimSpace(name)

	var exact, folded []T

	for _, it := range items {
		n := nameOf(it)

		if n == name {
			exact = append(exact, it)
		} else if strings.EqualFold(n, name) {
			folded = append(folded, it)
		}
	}

	if len(exact) > 0 {
		return exact
	}

	return folded
}

// refCandidate is one row of an ambiguity error's details
type refCandidate struct {
	Id     string `json:"id"`
	Name   string `json:"name"`
	Parent string `json:"parent,omitempty"`
}

// refResolveOne resolves an <id|name> reference among items. "id:<n>" and "name:<s>" force the
// interpretation; a bare string of 8+ digits is an id; anything else is a name.
func refResolveOne[T any](noun, ref string, items []T, idOf func(T) int64, nameOf func(T) string, parentOf func(T) string, listHint string) (T, error) {
	var zero T
	ref = strings.TrimSpace(ref)
	forceId, forceName := false, false

	switch {
	case strings.HasPrefix(ref, "id:"):
		ref, forceId = strings.TrimSpace(ref[3:]), true
	case strings.HasPrefix(ref, "name:"):
		ref, forceName = strings.TrimSpace(ref[5:]), true
	}

	if ref == "" {
		return zero, Invalid("pass the "+noun+"'s id or name — "+listHint, "an empty %s reference", noun)
	}

	if !forceName && refDigitsRe.MatchString(ref) {
		if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
			for _, it := range items {
				if idOf(it) == id {
					return it, nil
				}
			}
		}

		if forceId || len(ref) >= 8 {
			return zero, NotFound(listHint, "no %s with id %s", noun, ref).WithDetails(map[string]any{"id": ref})
		}
	}

	if forceId {
		return zero, Invalid("ids are the decimal strings the list routes return", "%q is not an id", ref)
	}

	matches := refMatchByName(items, ref, nameOf)

	switch len(matches) {
	case 0:
		return zero, NotFound(listHint, "no %s named %q", noun, ref).WithDetails(map[string]any{"name": ref})
	case 1:
		return matches[0], nil
	}

	candidates := make([]refCandidate, 0, len(matches))

	for _, m := range matches {
		c := refCandidate{Id: idString(idOf(m)), Name: nameOf(m)}

		if parentOf != nil {
			c.Parent = parentOf(m)
		}

		candidates = append(candidates, c)
	}

	return zero, Invalid("pass the id instead of the name (one of the candidates)", "%q matches %d %ss", ref, len(matches), noun).WithDetails(map[string]any{"candidates": candidates})
}

// refRefArg turns a pair of <x>_id / <x>_name arguments into one reference ("" when neither is set)
func refRefArg(base, id, name string) (string, error) {
	id, name = strings.TrimSpace(id), strings.TrimSpace(name)

	if id != "" && name != "" {
		return "", Invalid("pass "+base+"_id or "+base+"_name, not both", "both %s_id and %s_name were given", base, base)
	}

	if id != "" {
		if !refDigitsRe.MatchString(id) {
			return "", Invalid("ids are the decimal strings the list routes return; pass a name as "+base+"_name", "%s_id %q is not an id", base, id)
		}

		return "id:" + id, nil
	}

	if name != "" {
		return "name:" + name, nil
	}

	return "", nil
}

// refFieldChange is one row of a preview: the exact before and after of one field
type refFieldChange struct {
	Id    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Field string `json:"field"`
	From  any    `json:"from"`
	To    any    `json:"to"`
}

// refSibling is one entity in a display-order group
type refSibling struct {
	Id    int64
	Name  string
	Order int32
}

// refOrderChange is one display-order change of a move
type refOrderChange struct {
	Id   string `json:"id"`
	Name string `json:"name"`
	From int32  `json:"from"`
	To   int32  `json:"to"`
}

// refOrderPair is the stored form of a display order (journal payloads)
type refOrderPair struct {
	Id           string `json:"id"`
	DisplayOrder int32  `json:"displayOrder"`
}

// refComputeMove moves targetId to toIndex (0-based) among its siblings and renumbers the group
// 1..n, returning only the rows whose order changes. Siblings are ordered by (order, id).
func refComputeMove(sibs []refSibling, targetId int64, toIndex int) ([]refOrderChange, error) {
	ordered := append([]refSibling(nil), sibs...)

	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Order != ordered[j].Order {
			return ordered[i].Order < ordered[j].Order
		}

		return ordered[i].Id < ordered[j].Id
	})

	pos := -1

	for i, s := range ordered {
		if s.Id == targetId {
			pos = i
			break
		}
	}

	if pos < 0 {
		return nil, NotFound("list the group again; the item may have moved to another group", "the item is not among its siblings")
	}

	if toIndex < 0 || toIndex >= len(ordered) {
		return nil, Invalid(fmt.Sprintf("to_index is 0-based: 0 … %d", len(ordered)-1), "to_index %d is outside the group of %d", toIndex, len(ordered))
	}

	target := ordered[pos]
	rest := append(append([]refSibling(nil), ordered[:pos]...), ordered[pos+1:]...)
	moved := make([]refSibling, 0, len(ordered))
	moved = append(moved, rest[:toIndex]...)
	moved = append(moved, target)
	moved = append(moved, rest[toIndex:]...)

	var changes []refOrderChange

	for i, s := range moved {
		newOrder := int32(i + 1)

		if s.Order != newOrder {
			changes = append(changes, refOrderChange{Id: idString(s.Id), Name: s.Name, From: s.Order, To: newOrder})
		}
	}

	return changes, nil
}

// refMoveBody is the upstream move request shared by every family ({newDisplayOrders: [...]})
func refMoveBody(orders []refOrderPair) map[string]any {
	list := make([]map[string]any, 0, len(orders))

	for _, o := range orders {
		list = append(list, map[string]any{"id": o.Id, "displayOrder": o.DisplayOrder})
	}

	return map[string]any{"newDisplayOrders": list}
}

// refDB is the bound user's user-data database
func refDB(mc *Ctx) *datastore.Database {
	return datastore.Container.UserDataStore.Choose(mc.Uid)
}

// refParseIds parses a list of decimal id strings
func refParseIds(ids []string) ([]int64, error) {
	out := make([]int64, 0, len(ids))

	for _, s := range ids {
		n, err := ResolveId("id", s)

		if err != nil {
			return nil, err
		}

		out = append(out, n)
	}

	return out, nil
}

// refIdStrings renders ids for the wire
func refIdStrings(ids []int64) []string {
	out := make([]string, len(ids))

	for i, id := range ids {
		out[i] = idString(id)
	}

	return out
}

// refIdem makes create routes idempotent within the process lifetime (apis.mdx §7.6): a repeat
// with the same idempotency_key returns the original result, marked meta.replayed
var refIdem = struct {
	sync.Mutex
	m map[string]any
}{m: map[string]any{}}

func refIdemKey(mc *Ctx, key string) string {
	if key == "" {
		return ""
	}

	return strconv.FormatInt(mc.Uid, 10) + "|" + mc.Route.Method + " " + mc.Route.Path + "|" + key
}

func refIdemGet(mc *Ctx, key string) (any, bool) {
	k := refIdemKey(mc, key)

	if k == "" {
		return nil, false
	}

	refIdem.Lock()
	defer refIdem.Unlock()

	v, ok := refIdem.m[k]

	if ok {
		mc.SetMeta("replayed", true)
	}

	return v, ok
}

func refIdemPut(mc *Ctx, key string, v any) {
	k := refIdemKey(mc, key)

	if k == "" {
		return
	}

	refIdem.Lock()
	defer refIdem.Unlock()

	if len(refIdem.m) > 10000 {
		refIdem.m = map[string]any{}
	}

	refIdem.m[k] = v
}

// refCheckIdemKey validates an idempotency key (≤ 128 chars)
func refCheckIdemKey(key string) error {
	if len(key) > 128 {
		return Invalid("keep idempotency_key to 128 characters", "idempotency_key is %d characters", len(key))
	}

	return nil
}

// refCreatedId decodes the id of an upstream create response
func refCreatedId(v any) (int64, error) {
	data, err := json.Marshal(v)

	if err != nil {
		return 0, err
	}

	var out struct {
		Id string `json:"id"`
	}

	if err := json.Unmarshal(data, &out); err != nil || out.Id == "" {
		return 0, NewFail(CodeUpstreamError, "list the entities to see whether it was created", "the server did not return the new id")
	}

	return strconv.ParseInt(out.Id, 10, 64)
}

// ---------------------------------------------------------------------------------------------
// the kind registry: hide / move / delete / undelete for every family, and their inverses
// ---------------------------------------------------------------------------------------------

// refRow is the stored state of one entity, as the inverse executors check it
type refRow struct {
	Id      int64
	Name    string
	Hidden  bool
	Order   int32
	Deleted bool
}

// refKind describes one family for the generic hide/move/delete/undelete machinery
type refKind struct {
	Name string // account, category, tag, tag_group, template, insight
	List string // where to look, for hints
	// Hide / Move / Delete are the upstream handlers
	Hide   core.ApiHandlerFunc
	Move   core.ApiHandlerFunc
	Delete func(mc *Ctx, id int64) error
	// Rows reads the stored state of the given ids, deleted ones included
	Rows func(mc *Ctx, ids []int64) (map[int64]refRow, error)
	// Undelete restores soft-deleted rows (only rows the journal names)
	Undelete func(mc *Ctx, ids []int64) (int, error)
}

var refKinds = map[string]*refKind{}

// refRegisterKind registers a kind and its four generic inverse executors
func refRegisterKind(k *refKind) {
	refKinds[k.Name] = k

	if k.Hide != nil {
		RegisterInverse("ref.hide_"+k.Name, func(mc *Ctx, payload, check json.RawMessage) error {
			return refInvHide(mc, k, payload, check)
		})
	}

	if k.Move != nil {
		RegisterInverse("ref.set_"+k.Name+"_orders", func(mc *Ctx, payload, check json.RawMessage) error {
			return refInvOrders(mc, k, payload, check)
		})
	}

	if k.Delete != nil {
		RegisterInverse("ref.delete_"+k.Name, func(mc *Ctx, payload, check json.RawMessage) error {
			return refInvDelete(mc, k, payload)
		})
	}

	if k.Undelete != nil {
		RegisterInverse("ref.undelete_"+k.Name, func(mc *Ctx, payload, check json.RawMessage) error {
			return refInvUndelete(mc, k, payload)
		})
	}
}

type refHidePayload struct {
	Id     string `json:"id"`
	Hidden bool   `json:"hidden"`
}

type refIdsPayload struct {
	Ids []string `json:"ids"`
}

type refOrdersPayload struct {
	Orders []refOrderPair `json:"orders"`
}

func refInvHide(mc *Ctx, k *refKind, payload, check json.RawMessage) error {
	var p, c refHidePayload

	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}

	id, err := ResolveId("id", p.Id)

	if err != nil {
		return err
	}

	rows, err := k.Rows(mc, []int64{id})

	if err != nil {
		return err
	}

	row, ok := rows[id]

	if !ok || row.Deleted {
		return Conflict("nothing to undo: it no longer exists", "the %s %s was deleted since", k.Name, p.Id)
	}

	if len(check) > 0 {
		if err := json.Unmarshal(check, &c); err == nil && row.Hidden != c.Hidden {
			return Conflict("the "+k.Name+" was hidden or shown again since; nothing was changed", "the %s %s changed since the write", k.Name, p.Id)
		}
	}

	if row.Hidden == p.Hidden {
		return nil
	}

	_, err = mc.CallUpstream(k.Hide, "POST", nil, map[string]any{"id": p.Id, "hidden": p.Hidden})

	return err
}

func refInvOrders(mc *Ctx, k *refKind, payload, check json.RawMessage) error {
	var p, c refOrdersPayload

	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}

	if len(p.Orders) == 0 {
		return nil
	}

	ids := make([]int64, 0, len(p.Orders))

	for _, o := range p.Orders {
		id, err := ResolveId("id", o.Id)

		if err != nil {
			return err
		}

		ids = append(ids, id)
	}

	rows, err := k.Rows(mc, ids)

	if err != nil {
		return err
	}

	if len(check) > 0 {
		if err := json.Unmarshal(check, &c); err == nil {
			for _, o := range c.Orders {
				id, _ := strconv.ParseInt(o.Id, 10, 64)
				row, ok := rows[id]

				if !ok || row.Deleted {
					return Conflict("an item of the group was deleted since; reorder by hand", "the %s %s no longer exists", k.Name, o.Id)
				}

				if row.Order != o.DisplayOrder {
					return Conflict("the order was changed again since; nothing was changed", "the %s order changed since the write", k.Name)
				}
			}
		}
	}

	_, err = mc.CallUpstream(k.Move, "POST", nil, refMoveBody(p.Orders))

	return err
}

func refInvDelete(mc *Ctx, k *refKind, payload json.RawMessage) error {
	var p refIdsPayload

	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}

	ids, err := refParseIds(p.Ids)

	if err != nil {
		return err
	}

	rows, err := k.Rows(mc, ids)

	if err != nil {
		return err
	}

	for _, id := range ids {
		row, ok := rows[id]

		if !ok || row.Deleted {
			continue // already gone: a retried undo terminates
		}

		if err := k.Delete(mc, id); err != nil {
			f := toFail(err)

			return Conflict("it has been used since it was created (transactions, templates or tags now refer to it); remove those first, or leave it", "cannot remove the %s %s: %s", k.Name, idString(id), f.Message)
		}
	}

	return nil
}

func refInvUndelete(mc *Ctx, k *refKind, payload json.RawMessage) error {
	var p refIdsPayload

	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}

	ids, err := refParseIds(p.Ids)

	if err != nil {
		return err
	}

	_, err = k.Undelete(mc, ids)

	return err
}

// refUndeleteGeneric restores soft-deleted rows of a table with deleted/deleted_unix_time columns
func refUndeleteGeneric(mc *Ctx, bean any, idCol string, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	n, err := refDB(mc).NewSession(mc.Web).Cols("deleted", "deleted_unix_time").Where("uid=? AND deleted=?", mc.Uid, true).In(idCol, ids).Update(bean)

	if err != nil {
		return 0, err
	}

	return int(n), nil
}

// ---------------------------------------------------------------------------------------------
// generic write routes: hide, move, delete
// ---------------------------------------------------------------------------------------------

type refHideReq struct {
	WriteOpts
	Hidden *bool `json:"hidden"`
}

type refMoveReq struct {
	WriteOpts
	ToIndex *int `json:"to_index"`
}

type refDeleteReq struct {
	WriteOpts
}

// refHideTarget is what a family's resolver tells the generic hide route
type refHideTarget struct {
	Id       int64
	Name     string
	Hidden   bool
	Warnings []string
	// Extra is merged into the apply result (e.g. a schedule's "hiding does not pause it")
	Extra map[string]any
}

// refHandleHide is POST /<family>/:id/hide — body {hidden (default true)}
func refHandleHide(mc *Ctx, k *refKind, target func(mc *Ctx, ref string, hidden bool) (*refHideTarget, error)) (any, error) {
	var req refHideReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	hidden := true

	if req.Hidden != nil {
		hidden = *req.Hidden
	}

	ref := mc.Param("id")

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		t, err := target(mc, ref, hidden)

		if err != nil {
			return nil, err
		}

		plan := &Plan{State: t, Warnings: t.Warnings}

		if t.Hidden == hidden {
			plan.Changes = map[string]int{"update": 0, "unchanged": 1}
			plan.Preview = []refFieldChange{}
		} else {
			plan.Changes = map[string]int{"update": 1}
			plan.Count = 1
			plan.Preview = []refFieldChange{{Id: idString(t.Id), Name: t.Name, Field: "hidden", From: t.Hidden, To: hidden}}
		}

		return plan, nil
	}, func(p *Plan) (any, error) {
		t := p.State.(*refHideTarget)
		out := map[string]any{"id": idString(t.Id), "hidden": hidden, "changed": p.Count}

		for k, v := range t.Extra {
			out[k] = v
		}

		if p.Count == 0 {
			return out, nil
		}

		if _, err := mc.CallUpstream(k.Hide, "POST", nil, map[string]any{"id": idString(t.Id), "hidden": hidden}); err != nil {
			return nil, err
		}

		op := NewInverseOp("ref.hide_"+k.Name, refHidePayload{Id: idString(t.Id), Hidden: t.Hidden}, refHidePayload{Id: idString(t.Id), Hidden: hidden})
		redo := NewInverseOp("ref.hide_"+k.Name, refHidePayload{Id: idString(t.Id), Hidden: hidden}, refHidePayload{Id: idString(t.Id), Hidden: t.Hidden})
		op.Redo = &redo

		verb := "hide"

		if !hidden {
			verb = "unhide"
		}

		if _, err := RecordJournal(mc, verb+" "+k.Name+" "+idString(t.Id), 1, []InverseOp{op}); err != nil {
			return nil, err
		}

		return out, nil
	})
}

// refHandleMove is POST /<family>/:id/move — body {to_index} (0-based among its siblings)
func refHandleMove(mc *Ctx, k *refKind, siblings func(mc *Ctx, ref string) (int64, []refSibling, error)) (any, error) {
	var req refMoveReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if req.ToIndex == nil {
		return nil, Invalid("pass to_index, the 0-based position among its siblings", "to_index is required")
	}

	ref := mc.Param("id")

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		targetId, sibs, err := siblings(mc, ref)

		if err != nil {
			return nil, err
		}

		changes, err := refComputeMove(sibs, targetId, *req.ToIndex)

		if err != nil {
			return nil, err
		}

		if changes == nil {
			changes = []refOrderChange{}
		}

		plan := &Plan{Preview: changes, State: changes, Count: 0, Changes: map[string]int{"update": len(changes)}}

		if len(changes) > 0 {
			plan.Count = 1 // one move, however many rows renumber
		} else {
			plan.Changes["unchanged"] = 1
		}

		return plan, nil
	}, func(p *Plan) (any, error) {
		changes := p.State.([]refOrderChange)

		if len(changes) == 0 {
			return map[string]any{"moved": 0, "orders": changes}, nil
		}

		after := make([]refOrderPair, 0, len(changes))
		before := make([]refOrderPair, 0, len(changes))

		for _, c := range changes {
			after = append(after, refOrderPair{Id: c.Id, DisplayOrder: c.To})
			before = append(before, refOrderPair{Id: c.Id, DisplayOrder: c.From})
		}

		if _, err := mc.CallUpstream(k.Move, "POST", nil, refMoveBody(after)); err != nil {
			return nil, err
		}

		op := NewInverseOp("ref.set_"+k.Name+"_orders", refOrdersPayload{Orders: before}, refOrdersPayload{Orders: after})
		redo := NewInverseOp("ref.set_"+k.Name+"_orders", refOrdersPayload{Orders: after}, refOrdersPayload{Orders: before})
		op.Redo = &redo

		if _, err := RecordJournal(mc, "move "+k.Name+" "+changes[0].Id, 1, []InverseOp{op}); err != nil {
			return nil, err
		}

		return map[string]any{"moved": 1, "orders": changes}, nil
	})
}

// refDeleteTarget is what a family's resolver tells the generic delete route
type refDeleteTarget struct {
	Id int64
	// Ids are every row the delete soft-deletes (the entity and its children), for the undo
	Ids      []int64
	Preview  any
	Warnings []string
}

// refHandleDelete is DELETE /<family>/:id (admin tier). A delete of something already gone
// answers ok with deleted: 0, so a retry loop terminates (apis.mdx §7.6).
func refHandleDelete(mc *Ctx, k *refKind, ref string, target func(mc *Ctx, ref string) (*refDeleteTarget, error)) (any, error) {
	var req refDeleteReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	ref = strings.TrimSpace(ref)
	idText := strings.TrimPrefix(ref, "id:")

	if refDigitsRe.MatchString(idText) && (len(idText) >= 8 || strings.HasPrefix(ref, "id:")) {
		if id, err := strconv.ParseInt(idText, 10, 64); err == nil {
			rows, err := k.Rows(mc, []int64{id})

			if err != nil {
				return nil, err
			}

			if row, ok := rows[id]; ok && row.Deleted {
				return map[string]any{"id": idText, "deleted": 0, "note": "it was already deleted"}, nil
			}
		}
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		t, err := target(mc, ref)

		if err != nil {
			return nil, err
		}

		return &Plan{Changes: map[string]int{"delete": 1}, Count: 1, Preview: t.Preview, Warnings: t.Warnings, State: t}, nil
	}, func(p *Plan) (any, error) {
		t := p.State.(*refDeleteTarget)

		if err := k.Delete(mc, t.Id); err != nil {
			return nil, err
		}

		ids := t.Ids

		if len(ids) == 0 {
			ids = []int64{t.Id}
		}

		op := NewInverseOp("ref.undelete_"+k.Name, refIdsPayload{Ids: refIdStrings(ids)}, nil)
		redo := NewInverseOp("ref.delete_"+k.Name, refIdsPayload{Ids: []string{idString(t.Id)}}, nil)
		op.Redo = &redo

		if _, err := RecordJournal(mc, "delete "+k.Name+" "+idString(t.Id), 1, []InverseOp{op}); err != nil {
			return nil, err
		}

		return map[string]any{"id": idString(t.Id), "deleted": 1, "ids": refIdStrings(ids), "note": "a soft delete; POST /machine/v1/undo restores it"}, nil
	})
}

// refJournalCreate records the inverse of a create: undo deletes exactly the created rows, redo
// restores the same rows (same ids)
func refJournalCreate(mc *Ctx, kind string, deleteIds []int64, allIds []int64, summary string) error {
	if len(deleteIds) == 0 {
		return nil
	}

	if len(allIds) == 0 {
		allIds = deleteIds
	}

	op := NewInverseOp("ref.delete_"+kind, refIdsPayload{Ids: refIdStrings(deleteIds)}, nil)
	redo := NewInverseOp("ref.undelete_"+kind, refIdsPayload{Ids: refIdStrings(allIds)}, nil)
	op.Redo = &redo

	_, err := RecordJournal(mc, summary, len(allIds), []InverseOp{op})

	return err
}

// refJournalRestore records a field update: undo restores `before` if the row still holds `after`
func refJournalRestore(mc *Ctx, kind string, before, after any, summary string) error {
	op := NewInverseOp(kind, before, after)
	redo := NewInverseOp(kind, after, before)
	op.Redo = &redo

	_, err := RecordJournal(mc, summary, 1, []InverseOp{op})

	return err
}

// refCheckMatches compares the stored state against a journal check (both marshalled to JSON so a
// pointer-free comparison is exact)
func refCheckMatches(current any, check json.RawMessage) bool {
	if len(check) == 0 {
		return true
	}

	var want, have any

	decode := func(data []byte, out *any) error {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()

		return dec.Decode(out)
	}

	if err := decode(check, &want); err != nil {
		return false
	}

	data, err := json.Marshal(current)

	if err != nil {
		return false
	}

	if err := decode(data, &have); err != nil {
		return false
	}

	wantData, _ := json.Marshal(want)
	haveData, _ := json.Marshal(have)

	return bytes.Equal(wantData, haveData)
}

// ---------------------------------------------------------------------------------------------
// categories (§10.4)
// ---------------------------------------------------------------------------------------------

var refCategoryTypeNames = map[models.TransactionCategoryType]string{
	models.CATEGORY_TYPE_INCOME:   "income",
	models.CATEGORY_TYPE_EXPENSE:  "expense",
	models.CATEGORY_TYPE_TRANSFER: "transfer",
}

// refCategoryType parses income|expense|transfer (or 1|2|3); "" is 0
func refCategoryType(v string) (models.TransactionCategoryType, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return 0, nil
	case "income", "1":
		return models.CATEGORY_TYPE_INCOME, nil
	case "expense", "2":
		return models.CATEGORY_TYPE_EXPENSE, nil
	case "transfer", "3":
		return models.CATEGORY_TYPE_TRANSFER, nil
	}

	return 0, Invalid("type is income, expense or transfer", "unknown category type %q", v)
}

// refCategoryView is one category on the wire
type refCategoryView struct {
	Id            string             `json:"id"`
	Name          string             `json:"name"`
	ParentId      string             `json:"parentId"`
	ParentName    string             `json:"parentName,omitempty"`
	Level         string             `json:"level"`
	Type          string             `json:"type"`
	TypeCode      int                `json:"typeCode"`
	Icon          string             `json:"icon"`
	IconType      int                `json:"iconType"`
	Color         string             `json:"color"`
	Comment       string             `json:"comment"`
	DisplayOrder  int32              `json:"displayOrder"`
	Hidden        bool               `json:"hidden"`
	SubCategories []*refCategoryView `json:"subCategories,omitempty"`
}

func refLoadCategories(mc *Ctx) ([]*models.TransactionCategory, error) {
	cats, err := services.TransactionCategories.GetAllCategoriesByUid(mc.Web, mc.Uid, 0, -1)

	if err != nil {
		return nil, err
	}

	return cats, nil
}

func refCategoryName(c *models.TransactionCategory) string { return c.Name }
func refCategoryId(c *models.TransactionCategory) int64    { return c.CategoryId }

func refCategoryParentOf(all []*models.TransactionCategory) func(c *models.TransactionCategory) string {
	byId := map[int64]*models.TransactionCategory{}

	for _, c := range all {
		byId[c.CategoryId] = c
	}

	return func(c *models.TransactionCategory) string {
		if p, ok := byId[c.ParentCategoryId]; ok {
			return p.Name
		}

		return ""
	}
}

func refResolveCategory(mc *Ctx, all []*models.TransactionCategory, ref string) (*models.TransactionCategory, error) {
	return refResolveOne("category", ref, all, refCategoryId, refCategoryName, refCategoryParentOf(all), "GET /machine/v1/categories?include_hidden=true lists them")
}

// refCategoryViewOf renders one category (no children)
func refCategoryViewOf(c *models.TransactionCategory, parentName string) *refCategoryView {
	level := "primary"

	if c.ParentCategoryId != models.LevelOneTransactionCategoryParentId {
		level = "secondary"
	}

	return &refCategoryView{
		Id:           idString(c.CategoryId),
		Name:         c.Name,
		ParentId:     idString(c.ParentCategoryId),
		ParentName:   parentName,
		Level:        level,
		Type:         refCategoryTypeNames[c.Type],
		TypeCode:     int(c.Type),
		Icon:         strconv.FormatInt(c.Icon, 10),
		IconType:     int(c.IconType),
		Color:        c.Color,
		Comment:      c.Comment,
		DisplayOrder: c.DisplayOrder,
		Hidden:       c.Hidden,
	}
}

// refCategoryTree renders primaries with their secondaries nested, ordered by (type, display order)
func refCategoryTree(all []*models.TransactionCategory, includeHidden bool, typ models.TransactionCategoryType) []*refCategoryView {
	byId := map[int64]*models.TransactionCategory{}

	for _, c := range all {
		byId[c.CategoryId] = c
	}

	var primaries []*models.TransactionCategory
	children := map[int64][]*models.TransactionCategory{}

	for _, c := range all {
		if typ != 0 && c.Type != typ {
			continue
		}

		if !includeHidden && c.Hidden {
			continue
		}

		if c.ParentCategoryId == models.LevelOneTransactionCategoryParentId {
			primaries = append(primaries, c)
		} else {
			children[c.ParentCategoryId] = append(children[c.ParentCategoryId], c)
		}
	}

	byOrder := func(list []*models.TransactionCategory) {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Type != list[j].Type {
				return list[i].Type < list[j].Type
			}

			if list[i].DisplayOrder != list[j].DisplayOrder {
				return list[i].DisplayOrder < list[j].DisplayOrder
			}

			return list[i].CategoryId < list[j].CategoryId
		})
	}

	byOrder(primaries)
	out := make([]*refCategoryView, 0, len(primaries))

	for _, p := range primaries {
		v := refCategoryViewOf(p, "")
		subs := children[p.CategoryId]
		byOrder(subs)

		for _, s := range subs {
			v.SubCategories = append(v.SubCategories, refCategoryViewOf(s, p.Name))
		}

		out = append(out, v)
	}

	return out
}

func refHandleCategoryList(mc *Ctx) (any, error) {
	typ, err := refCategoryType(mc.Query("type"))

	if err != nil {
		return nil, err
	}

	includeHidden, err := mc.QueryBool("include_hidden", false)

	if err != nil {
		return nil, err
	}

	flat, err := mc.QueryBool("flat", false)

	if err != nil {
		return nil, err
	}

	all, err := refLoadCategories(mc)

	if err != nil {
		return nil, err
	}

	parentOf := refCategoryParentOf(all)
	filters := map[string]any{"type": refCategoryTypeNames[typ], "includeHidden": includeHidden}

	// a name or parent filter answers flat rows, so a secondary can be found by name
	if name := mc.Query("name"); name != "" || mc.Query("parent_id") != "" {
		var pool []*models.TransactionCategory

		for _, c := range all {
			if (typ == 0 || c.Type == typ) && (includeHidden || !c.Hidden) {
				pool = append(pool, c)
			}
		}

		if pref := mc.Query("parent_id"); pref != "" {
			parent, err := refResolveCategory(mc, all, pref)

			if err != nil {
				return nil, err
			}

			var kids []*models.TransactionCategory

			for _, c := range pool {
				if c.ParentCategoryId == parent.CategoryId {
					kids = append(kids, c)
				}
			}

			pool = kids
			filters["parentId"] = idString(parent.CategoryId)
		}

		if name != "" {
			pool = refMatchByName(pool, name, refCategoryName)
			filters["name"] = name
		}

		sort.SliceStable(pool, func(i, j int) bool {
			if pool[i].DisplayOrder != pool[j].DisplayOrder {
				return pool[i].DisplayOrder < pool[j].DisplayOrder
			}

			return pool[i].CategoryId < pool[j].CategoryId
		})

		rows := make([]*refCategoryView, 0, len(pool))

		for _, c := range pool {
			rows = append(rows, refCategoryViewOf(c, parentOf(c)))
		}

		return map[string]any{"categories": rows, "count": len(rows), "filters": filters}, nil
	}

	tree := refCategoryTree(all, includeHidden, typ)

	if flat {
		rows := make([]*refCategoryView, 0, len(all))

		for _, p := range tree {
			subs := p.SubCategories
			p.SubCategories = nil
			rows = append(rows, p)
			rows = append(rows, subs...)
		}

		filters["flat"] = true

		return map[string]any{"categories": rows, "count": len(rows), "filters": filters}, nil
	}

	count := 0

	for _, p := range tree {
		count += 1 + len(p.SubCategories)
	}

	return map[string]any{"categories": tree, "count": count, "primaryCount": len(tree), "filters": filters}, nil
}

func refHandleCategoryGet(mc *Ctx) (any, error) {
	all, err := refLoadCategories(mc)

	if err != nil {
		return nil, err
	}

	c, err := refResolveCategory(mc, all, mc.Param("id"))

	if err != nil {
		return nil, err
	}

	v := refCategoryViewOf(c, refCategoryParentOf(all)(c))

	if c.ParentCategoryId == models.LevelOneTransactionCategoryParentId {
		for _, p := range refCategoryTree(all, true, c.Type) {
			if p.Id == v.Id {
				v.SubCategories = p.SubCategories
			}
		}
	}

	return map[string]any{"category": v}, nil
}

// refCategoryCreateReq is POST /categories
type refCategoryCreateReq struct {
	WriteOpts
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	ParentId   string  `json:"parent_id"`
	ParentName string  `json:"parent_name"`
	Color      string  `json:"color"`
	Icon       refFlex `json:"icon"`
	Comment    string  `json:"comment"`
}

type refCategoryCreatePlan struct {
	Request models.TransactionCategoryCreateRequest
	Parent  *models.TransactionCategory
}

func refPlanCategoryCreate(all []*models.TransactionCategory, req *refCategoryCreateReq, resolve func(ref string) (*models.TransactionCategory, error)) (*refCategoryCreatePlan, map[string]any, []string, error) {
	name, err := refName("category", req.Name)

	if err != nil {
		return nil, nil, nil, err
	}

	typ, err := refCategoryType(req.Type)

	if err != nil {
		return nil, nil, nil, err
	}

	pref, err := refRefArg("parent", req.ParentId, req.ParentName)

	if err != nil {
		return nil, nil, nil, err
	}

	var parent *models.TransactionCategory

	if pref != "" {
		parent, err = resolve(pref)

		if err != nil {
			return nil, nil, nil, err
		}

		if parent.ParentCategoryId != models.LevelOneTransactionCategoryParentId {
			return nil, nil, nil, Invalid("categories have two levels: pass a PRIMARY category as the parent", "%q is itself a secondary category", parent.Name)
		}

		if typ == 0 {
			typ = parent.Type
		} else if typ != parent.Type {
			return nil, nil, nil, Invalid("a secondary category takes its parent's type; omit type or pick a parent of that type", "the parent %q is a %s category, not %s", parent.Name, refCategoryTypeNames[parent.Type], refCategoryTypeNames[typ])
		}
	}

	if typ == 0 {
		return nil, nil, nil, Invalid("pass type (income, expense or transfer) for a primary category", "type is required for a primary category")
	}

	color, err := refColor(req.Color, refDefaultColor)

	if err != nil {
		return nil, nil, nil, err
	}

	icon, err := refIcon(req.Icon, refDefaultIcon)

	if err != nil {
		return nil, nil, nil, err
	}

	comment, err := refComment(req.Comment)

	if err != nil {
		return nil, nil, nil, err
	}

	var warnings []string
	parentId := int64(0)

	if parent != nil {
		parentId = parent.CategoryId
	} else {
		warnings = append(warnings, "this is a PRIMARY category; transactions use secondary categories, so add at least one under it")
	}

	for _, c := range all {
		if c.Type == typ && c.ParentCategoryId == parentId && strings.EqualFold(c.Name, name) {
			warnings = append(warnings, fmt.Sprintf("a %s category named %q already exists here (id %s)", refCategoryTypeNames[typ], c.Name, idString(c.CategoryId)))
		}
	}

	level := "primary"
	parentName := ""

	if parent != nil {
		level = "secondary"
		parentName = parent.Name
	}

	preview := map[string]any{
		"action": "create", "name": name, "type": refCategoryTypeNames[typ], "level": level,
		"parentId": idString(parentId), "parentName": parentName, "color": color, "icon": strconv.FormatInt(icon, 10), "comment": comment,
	}

	return &refCategoryCreatePlan{
		Request: models.TransactionCategoryCreateRequest{
			Name: name, Type: typ, ParentId: parentId, Icon: icon, IconType: core.ICON_TYPE_SYSTEM, Color: color, Comment: comment,
			ClientSessionId: req.IdempotencyKey,
		},
		Parent: parent,
	}, preview, warnings, nil
}

func refHandleCategoryCreate(mc *Ctx) (any, error) {
	var req refCategoryCreateReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if err := refCheckIdemKey(req.IdempotencyKey); err != nil {
		return nil, err
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		all, err := refLoadCategories(mc)

		if err != nil {
			return nil, err
		}

		plan, preview, warnings, err := refPlanCategoryCreate(all, &req, func(ref string) (*models.TransactionCategory, error) {
			return refResolveCategory(mc, all, ref)
		})

		if err != nil {
			return nil, err
		}

		return &Plan{Changes: map[string]int{"create": 1}, Count: 1, Preview: preview, Warnings: warnings, State: plan}, nil
	}, func(p *Plan) (any, error) {
		if v, ok := refIdemGet(mc, req.IdempotencyKey); ok {
			return v, nil
		}

		plan := p.State.(*refCategoryCreatePlan)
		res, err := mc.CallUpstream(api.TransactionCategories.CategoryCreateHandler, "POST", nil, plan.Request)

		if err != nil {
			return nil, err
		}

		id, err := refCreatedId(res)

		if err != nil {
			return nil, err
		}

		if err := refJournalCreate(mc, "category", []int64{id}, nil, "create category "+idString(id)); err != nil {
			return nil, err
		}

		out, err := refCategoryResult(mc, id)

		if err != nil {
			return nil, err
		}

		refIdemPut(mc, req.IdempotencyKey, out)

		return out, nil
	})
}

func refCategoryResult(mc *Ctx, id int64) (any, error) {
	all, err := refLoadCategories(mc)

	if err != nil {
		return nil, err
	}

	for _, c := range all {
		if c.CategoryId == id {
			return map[string]any{"category": refCategoryViewOf(c, refCategoryParentOf(all)(c))}, nil
		}
	}

	return map[string]any{"category": map[string]any{"id": idString(id)}}, nil
}

// refCategoryBatchReq is POST /categories/batch — primaries with their secondaries
type refCategoryBatchReq struct {
	WriteOpts
	Categories []refCategoryBatchItem `json:"categories"`
}

type refCategoryBatchItem struct {
	Name          string                `json:"name"`
	Type          string                `json:"type"`
	Color         string                `json:"color"`
	Icon          refFlex               `json:"icon"`
	Comment       string                `json:"comment"`
	SubCategories []refCategoryBatchSub `json:"sub_categories"`
}

type refCategoryBatchSub struct {
	Name    string  `json:"name"`
	Color   string  `json:"color"`
	Icon    refFlex `json:"icon"`
	Comment string  `json:"comment"`
}

func refPlanCategoryBatch(all []*models.TransactionCategory, items []refCategoryBatchItem) (*models.TransactionCategoryCreateBatchRequest, []map[string]any, []string, int, error) {
	if len(items) == 0 {
		return nil, nil, nil, 0, Invalid("pass categories: [{name, type, sub_categories: [{name}]}]", "no categories to create")
	}

	out := &models.TransactionCategoryCreateBatchRequest{}
	var preview []map[string]any
	var warnings []string
	total := 0

	for i, it := range items {
		name, err := refName("category", it.Name)

		if err != nil {
			return nil, nil, nil, 0, Invalid("categories["+strconv.Itoa(i)+"]: "+err.(*Fail).Hint, "%s", err.(*Fail).Message)
		}

		typ, err := refCategoryType(it.Type)

		if err != nil {
			return nil, nil, nil, 0, err
		}

		if typ == 0 {
			return nil, nil, nil, 0, Invalid("each primary needs type income, expense or transfer", "categories[%d] %q has no type", i, name)
		}

		color, err := refColor(it.Color, refDefaultColor)

		if err != nil {
			return nil, nil, nil, 0, err
		}

		icon, err := refIcon(it.Icon, refDefaultIcon)

		if err != nil {
			return nil, nil, nil, 0, err
		}

		comment, err := refComment(it.Comment)

		if err != nil {
			return nil, nil, nil, 0, err
		}

		for _, c := range all {
			if c.Type == typ && c.ParentCategoryId == 0 && strings.EqualFold(c.Name, name) {
				warnings = append(warnings, fmt.Sprintf("a primary %s category named %q already exists (id %s); this batch adds another", refCategoryTypeNames[typ], c.Name, idString(c.CategoryId)))
			}
		}

		primary := &models.TransactionCategoryCreateWithSubCategories{Name: name, Type: typ, Icon: icon, IconType: core.ICON_TYPE_SYSTEM, Color: color, Comment: comment, SubCategories: []*models.TransactionCategoryCreateRequest{}}
		subNames := make([]string, 0, len(it.SubCategories))

		for j, s := range it.SubCategories {
			sname, err := refName("category", s.Name)

			if err != nil {
				return nil, nil, nil, 0, Invalid(fmt.Sprintf("categories[%d].sub_categories[%d] needs a name", i, j), "%s", err.(*Fail).Message)
			}

			scolor, err := refColor(s.Color, color)

			if err != nil {
				return nil, nil, nil, 0, err
			}

			sicon, err := refIcon(s.Icon, icon)

			if err != nil {
				return nil, nil, nil, 0, err
			}

			scomment, err := refComment(s.Comment)

			if err != nil {
				return nil, nil, nil, 0, err
			}

			primary.SubCategories = append(primary.SubCategories, &models.TransactionCategoryCreateRequest{Name: sname, Type: typ, Icon: sicon, IconType: core.ICON_TYPE_SYSTEM, Color: scolor, Comment: scomment})
			subNames = append(subNames, sname)
		}

		if len(subNames) == 0 {
			warnings = append(warnings, fmt.Sprintf("%q has no secondary categories; transactions cannot use a primary category", name))
		}

		out.Categories = append(out.Categories, primary)
		preview = append(preview, map[string]any{"action": "create", "name": name, "type": refCategoryTypeNames[typ], "subCategories": subNames})
		total += 1 + len(subNames)
	}

	return out, preview, warnings, total, nil
}

func refHandleCategoryBatch(mc *Ctx) (any, error) {
	var req refCategoryBatchReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		all, err := refLoadCategories(mc)

		if err != nil {
			return nil, err
		}

		batch, preview, warnings, total, err := refPlanCategoryBatch(all, req.Categories)

		if err != nil {
			return nil, err
		}

		return &Plan{Changes: map[string]int{"create": total}, Count: total, Preview: preview, Warnings: warnings, State: batch}, nil
	}, func(p *Plan) (any, error) {
		batch := p.State.(*models.TransactionCategoryCreateBatchRequest)
		before, err := refLoadCategories(mc)

		if err != nil {
			return nil, err
		}

		existed := map[int64]bool{}

		for _, c := range before {
			existed[c.CategoryId] = true
		}

		if _, err := mc.CallUpstream(api.TransactionCategories.CategoryCreateBatchHandler, "POST", nil, batch); err != nil {
			return nil, err
		}

		after, err := refLoadCategories(mc)

		if err != nil {
			return nil, err
		}

		var primaries, allIds []int64
		var created []*refCategoryView
		parentOf := refCategoryParentOf(after)

		for _, c := range after {
			if existed[c.CategoryId] {
				continue
			}

			allIds = append(allIds, c.CategoryId)
			created = append(created, refCategoryViewOf(c, parentOf(c)))

			if c.ParentCategoryId == 0 {
				primaries = append(primaries, c.CategoryId)
			}
		}

		if err := refJournalCreate(mc, "category", primaries, allIds, fmt.Sprintf("create %d categories", len(allIds))); err != nil {
			return nil, err
		}

		return map[string]any{"categories": created, "created": len(created)}, nil
	})
}

// refCategoryFields are the editable fields of a category (journal payloads)
type refCategoryFields struct {
	Id       string `json:"id"`
	Name     string `json:"name"`
	ParentId string `json:"parentId"`
	Color    string `json:"color"`
	Icon     int64  `json:"icon"`
	Comment  string `json:"comment"`
}

func refCategoryFieldsOf(c *models.TransactionCategory) refCategoryFields {
	return refCategoryFields{Id: idString(c.CategoryId), Name: c.Name, ParentId: idString(c.ParentCategoryId), Color: c.Color, Icon: c.Icon, Comment: c.Comment}
}

// refCategoryModify builds upstream's modify request: every field the category has, with f applied
func refCategoryModify(c *models.TransactionCategory, f refCategoryFields) (*models.TransactionCategoryModifyRequest, error) {
	parentId, err := strconv.ParseInt(f.ParentId, 10, 64)

	if err != nil {
		return nil, Invalid("parent ids are decimal strings", "parent id %q is invalid", f.ParentId)
	}

	return &models.TransactionCategoryModifyRequest{
		Id: c.CategoryId, Name: f.Name, ParentId: parentId, Icon: f.Icon, IconType: c.IconType, Color: f.Color, Comment: f.Comment, Hidden: c.Hidden,
	}, nil
}

type refCategoryPatchReq struct {
	WriteOpts
	Name       *string  `json:"name"`
	ParentId   *string  `json:"parent_id"`
	ParentName *string  `json:"parent_name"`
	Color      *string  `json:"color"`
	Icon       *refFlex `json:"icon"`
	Comment    *string  `json:"comment"`
	Type       *string  `json:"type"`
	Hidden     *bool    `json:"hidden"`
}

type refCategoryPatchState struct {
	Category *models.TransactionCategory
	Before   refCategoryFields
	After    refCategoryFields
}

func refHandleCategoryPatch(mc *Ctx) (any, error) {
	var req refCategoryPatchReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if req.Type != nil {
		return nil, Invalid("a category's type cannot change; create a new category of the other type and move the transactions (POST /machine/v1/transactions/set-category)", "type is not editable")
	}

	if req.Hidden != nil {
		return nil, Invalid("use POST /machine/v1/categories/:id/hide {hidden}", "hidden is set through the hide route")
	}

	ref := mc.Param("id")

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		all, err := refLoadCategories(mc)

		if err != nil {
			return nil, err
		}

		c, err := refResolveCategory(mc, all, ref)

		if err != nil {
			return nil, err
		}

		before := refCategoryFieldsOf(c)
		after := before
		var changes []refFieldChange
		add := func(field string, from, to any) {
			changes = append(changes, refFieldChange{Id: before.Id, Name: c.Name, Field: field, From: from, To: to})
		}

		if req.Name != nil {
			name, err := refName("category", *req.Name)

			if err != nil {
				return nil, err
			}

			if name != before.Name {
				after.Name = name
				add("name", before.Name, name)
			}
		}

		pid, pname := "", ""

		if req.ParentId != nil {
			pid = *req.ParentId
		}

		if req.ParentName != nil {
			pname = *req.ParentName
		}

		pref, err := refRefArg("parent", pid, pname)

		if err != nil {
			return nil, err
		}

		if pref != "" {
			if c.ParentCategoryId == 0 {
				return nil, Invalid("a primary category stays primary; only secondary categories move between parents", "%q is a primary category", c.Name)
			}

			parent, err := refResolveCategory(mc, all, pref)

			if err != nil {
				return nil, err
			}

			if parent.ParentCategoryId != 0 {
				return nil, Invalid("pass a PRIMARY category as the new parent", "%q is a secondary category", parent.Name)
			}

			if parent.Type != c.Type {
				return nil, Invalid("the new parent must be a "+refCategoryTypeNames[c.Type]+" category", "%q is a %s category", parent.Name, refCategoryTypeNames[parent.Type])
			}

			if parent.CategoryId != c.ParentCategoryId {
				after.ParentId = idString(parent.CategoryId)
				add("parentId", before.ParentId, after.ParentId)
			}
		}

		if req.Color != nil {
			color, err := refColor(*req.Color, before.Color)

			if err != nil {
				return nil, err
			}

			if color != before.Color {
				after.Color = color
				add("color", before.Color, color)
			}
		}

		if req.Icon != nil {
			icon, err := refIcon(*req.Icon, before.Icon)

			if err != nil {
				return nil, err
			}

			if icon != before.Icon {
				after.Icon = icon
				add("icon", strconv.FormatInt(before.Icon, 10), strconv.FormatInt(icon, 10))
			}
		}

		if req.Comment != nil {
			comment, err := refComment(*req.Comment)

			if err != nil {
				return nil, err
			}

			if comment != before.Comment {
				after.Comment = comment
				add("comment", before.Comment, comment)
			}
		}

		if changes == nil {
			changes = []refFieldChange{}
		}

		plan := &Plan{Preview: changes, State: &refCategoryPatchState{Category: c, Before: before, After: after}, Changes: map[string]int{"update": 0}}

		if len(changes) > 0 {
			plan.Changes["update"] = 1
			plan.Count = 1
		} else {
			plan.Changes["unchanged"] = 1
		}

		return plan, nil
	}, func(p *Plan) (any, error) {
		st := p.State.(*refCategoryPatchState)

		if p.Count == 0 {
			return map[string]any{"updated": 0, "category": refCategoryViewOf(st.Category, "")}, nil
		}

		if err := refApplyCategoryFields(mc, st.Category, st.After); err != nil {
			return nil, err
		}

		if err := refJournalRestore(mc, "ref.restore_category_fields", st.Before, st.After, "edit category "+st.Before.Id); err != nil {
			return nil, err
		}

		out, err := refCategoryResult(mc, st.Category.CategoryId)

		if err != nil {
			return nil, err
		}

		if m, ok := out.(map[string]any); ok {
			m["updated"] = 1
		}

		return out, nil
	})
}

func refApplyCategoryFields(mc *Ctx, c *models.TransactionCategory, f refCategoryFields) error {
	modify, err := refCategoryModify(c, f)

	if err != nil {
		return err
	}

	_, err = mc.CallUpstream(api.TransactionCategories.CategoryModifyHandler, "POST", nil, modify)

	return err
}

func refInvRestoreCategory(mc *Ctx, payload, check json.RawMessage) error {
	var want refCategoryFields

	if err := json.Unmarshal(payload, &want); err != nil {
		return err
	}

	id, err := ResolveId("id", want.Id)

	if err != nil {
		return err
	}

	c, err := services.TransactionCategories.GetCategoryByCategoryId(mc.Web, mc.Uid, id)

	if err != nil || c == nil {
		return Conflict("the category was deleted since; nothing to restore", "category %s no longer exists", want.Id)
	}

	if !refCheckMatches(refCategoryFieldsOf(c), check) {
		return Conflict("the category was edited again since (in the browser or by another write); nothing was changed", "category %s changed since the write", want.Id)
	}

	if refCheckMatches(refCategoryFieldsOf(c), refMustJSON(want)) {
		return nil
	}

	return refApplyCategoryFields(mc, c, want)
}

// mustJSON marshals a value that cannot fail to marshal
func refMustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

// refCategorySiblings is the display-order group of a category: same type, same parent
func refCategorySiblings(mc *Ctx, ref string) (int64, []refSibling, error) {
	all, err := refLoadCategories(mc)

	if err != nil {
		return 0, nil, err
	}

	c, err := refResolveCategory(mc, all, ref)

	if err != nil {
		return 0, nil, err
	}

	var sibs []refSibling

	for _, o := range all {
		if o.Type == c.Type && o.ParentCategoryId == c.ParentCategoryId {
			sibs = append(sibs, refSibling{Id: o.CategoryId, Name: o.Name, Order: o.DisplayOrder})
		}
	}

	return c.CategoryId, sibs, nil
}

func refCategoryHideTarget(mc *Ctx, ref string, hidden bool) (*refHideTarget, error) {
	all, err := refLoadCategories(mc)

	if err != nil {
		return nil, err
	}

	c, err := refResolveCategory(mc, all, ref)

	if err != nil {
		return nil, err
	}

	t := &refHideTarget{Id: c.CategoryId, Name: c.Name, Hidden: c.Hidden}

	if hidden && c.ParentCategoryId == 0 {
		t.Warnings = append(t.Warnings, "hiding a primary category hides it from pickers; its secondary categories keep their own hidden flags")
	}

	return t, nil
}

func refCategoryDeleteTarget(mc *Ctx, ref string) (*refDeleteTarget, error) {
	all, err := refLoadCategories(mc)

	if err != nil {
		return nil, err
	}

	c, err := refResolveCategory(mc, all, ref)

	if err != nil {
		return nil, err
	}

	ids := []int64{c.CategoryId}
	var subNames []string

	for _, o := range all {
		if o.ParentCategoryId == c.CategoryId && c.ParentCategoryId == 0 {
			ids = append(ids, o.CategoryId)
			subNames = append(subNames, o.Name)
		}
	}

	used, err := services.Transactions.GetTransactionCount(mc.Web, mc.Uid, 0, 0, 0, ids, nil, nil, false, "", "", core.MATCH_MODE_DEFAULT, false)

	if err != nil {
		return nil, err
	}

	if used > 0 {
		return nil, Conflict("move them to another category first (POST /machine/v1/transactions/set-category), or hide the category instead", "%d transactions use %q (or its secondary categories); a category in use cannot be deleted", used, c.Name).WithDetails(map[string]any{"transactions": used})
	}

	templates, err := refTemplatesUsing(mc, func(t *models.TransactionTemplate) bool {
		for _, id := range ids {
			if t.CategoryId == id {
				return true
			}
		}

		return false
	})

	if err != nil {
		return nil, err
	}

	if len(templates) > 0 {
		return nil, Conflict("edit or delete those templates first (GET /machine/v1/templates)", "%d templates or active schedules use %q", len(templates), c.Name).WithDetails(map[string]any{"templates": templates})
	}

	preview := map[string]any{"action": "delete", "id": idString(c.CategoryId), "name": c.Name, "type": refCategoryTypeNames[c.Type], "subCategories": subNames}

	return &refDeleteTarget{Id: c.CategoryId, Ids: ids, Preview: preview}, nil
}

func refCategoryRows(mc *Ctx, ids []int64) (map[int64]refRow, error) {
	var rows []*models.TransactionCategory

	if err := refDB(mc).NewSession(mc.Web).Where("uid=?", mc.Uid).In("category_id", ids).Find(&rows); err != nil {
		return nil, err
	}

	out := map[int64]refRow{}

	for _, r := range rows {
		out[r.CategoryId] = refRow{Id: r.CategoryId, Name: r.Name, Hidden: r.Hidden, Order: r.DisplayOrder, Deleted: r.Deleted}
	}

	return out, nil
}

var refCategoryKind = &refKind{
	Name: "category",
	List: "GET /machine/v1/categories",
	Rows: refCategoryRows,
	Delete: func(mc *Ctx, id int64) error {
		_, err := mc.CallUpstream(api.TransactionCategories.CategoryDeleteHandler, "POST", nil, map[string]any{"id": idString(id)})
		return err
	},
	Undelete: func(mc *Ctx, ids []int64) (int, error) {
		return refUndeleteGeneric(mc, &models.TransactionCategory{}, "category_id", ids)
	},
}

// refTemplatesUsing lists the non-deleted templates (and schedules that can still fire) matching fn,
// as upstream's delete guards count them
func refTemplatesUsing(mc *Ctx, fn func(t *models.TransactionTemplate) bool) ([]refCandidate, error) {
	var out []refCandidate

	for _, tt := range []models.TransactionTemplateType{models.TRANSACTION_TEMPLATE_TYPE_NORMAL, models.TRANSACTION_TEMPLATE_TYPE_SCHEDULE} {
		list, err := services.TransactionTemplates.GetAllTemplatesByUid(mc.Web, mc.Uid, tt)

		if err != nil {
			return nil, err
		}

		for _, t := range list {
			if tt == models.TRANSACTION_TEMPLATE_TYPE_SCHEDULE && t.ScheduledFrequencyType == models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DISABLED {
				continue
			}

			if fn(t) {
				out = append(out, refCandidate{Id: idString(t.TemplateId), Name: t.Name})
			}
		}
	}

	return out, nil
}

// ---------------------------------------------------------------------------------------------
// tags and tag groups (§10.5)
// ---------------------------------------------------------------------------------------------

// refMaxTagsPerTransaction is upstream's limit on tags per transaction and per template
const refMaxTagsPerTransaction = 10

type refTagView struct {
	Id           string `json:"id"`
	Name         string `json:"name"`
	GroupId      string `json:"groupId"`
	GroupName    string `json:"groupName,omitempty"`
	DisplayOrder int32  `json:"displayOrder"`
	Hidden       bool   `json:"hidden"`
}

type refTagGroupView struct {
	Id           string        `json:"id"`
	Name         string        `json:"name"`
	DisplayOrder int32         `json:"displayOrder"`
	TagCount     int           `json:"tagCount"`
	Tags         []*refTagView `json:"tags,omitempty"`
}

func refLoadTags(mc *Ctx) ([]*models.TransactionTag, error) {
	return services.TransactionTags.GetAllTagsByUid(mc.Web, mc.Uid)
}

func refLoadTagGroups(mc *Ctx) ([]*models.TransactionTagGroup, error) {
	return services.TransactionTagGroups.GetAllTagGroupsByUid(mc.Web, mc.Uid)
}

func refTagName(t *models.TransactionTag) string           { return t.Name }
func refTagId(t *models.TransactionTag) int64              { return t.TagId }
func refTagGroupName(g *models.TransactionTagGroup) string { return g.Name }
func refTagGroupId(g *models.TransactionTagGroup) int64    { return g.TagGroupId }

func refResolveTag(all []*models.TransactionTag, ref string) (*models.TransactionTag, error) {
	return refResolveOne("tag", ref, all, refTagId, refTagName, nil, "GET /machine/v1/tags?include_hidden=true lists them")
}

func refResolveTagGroup(all []*models.TransactionTagGroup, ref string) (*models.TransactionTagGroup, error) {
	return refResolveOne("tag group", ref, all, refTagGroupId, refTagGroupName, nil, "GET /machine/v1/tag-groups lists them")
}

// refTagGroupArg resolves group_id / group_name; "0" (or "none") means no group. set reports
// whether either was given.
func refTagGroupArg(groups []*models.TransactionTagGroup, id, name string) (groupId int64, set bool, err error) {
	id, name = strings.TrimSpace(id), strings.TrimSpace(name)

	if id == "0" || strings.EqualFold(id, "none") || (id == "" && strings.EqualFold(name, "none") && len(refMatchByName(groups, name, refTagGroupName)) == 0) {
		if id != "" && name != "" {
			return 0, false, Invalid("pass group_id or group_name, not both", "both group_id and group_name were given")
		}

		return 0, true, nil
	}

	ref, err := refRefArg("group", id, name)

	if err != nil || ref == "" {
		return 0, false, err
	}

	g, err := refResolveTagGroup(groups, ref)

	if err != nil {
		return 0, false, err
	}

	return g.TagGroupId, true, nil
}

func refTagViews(tags []*models.TransactionTag, groups []*models.TransactionTagGroup) []*refTagView {
	groupName := map[int64]string{}
	groupOrder := map[int64]int32{}

	for _, g := range groups {
		groupName[g.TagGroupId] = g.Name
		groupOrder[g.TagGroupId] = g.DisplayOrder
	}

	sorted := append([]*models.TransactionTag(nil), tags...)

	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]

		if a.TagGroupId != b.TagGroupId {
			if a.TagGroupId == 0 || b.TagGroupId == 0 {
				return a.TagGroupId == 0
			}

			if groupOrder[a.TagGroupId] != groupOrder[b.TagGroupId] {
				return groupOrder[a.TagGroupId] < groupOrder[b.TagGroupId]
			}

			return a.TagGroupId < b.TagGroupId
		}

		if a.DisplayOrder != b.DisplayOrder {
			return a.DisplayOrder < b.DisplayOrder
		}

		return a.TagId < b.TagId
	})

	out := make([]*refTagView, 0, len(sorted))

	for _, t := range sorted {
		out = append(out, &refTagView{Id: idString(t.TagId), Name: t.Name, GroupId: idString(t.TagGroupId), GroupName: groupName[t.TagGroupId], DisplayOrder: t.DisplayOrder, Hidden: t.Hidden})
	}

	return out
}

func refHandleTagList(mc *Ctx) (any, error) {
	includeHidden, err := mc.QueryBool("include_hidden", false)

	if err != nil {
		return nil, err
	}

	tags, err := refLoadTags(mc)

	if err != nil {
		return nil, err
	}

	groups, err := refLoadTagGroups(mc)

	if err != nil {
		return nil, err
	}

	filters := map[string]any{"includeHidden": includeHidden}
	var pool []*models.TransactionTag

	for _, t := range tags {
		if includeHidden || !t.Hidden {
			pool = append(pool, t)
		}
	}

	if gid, set, err := refTagGroupArg(groups, mc.Query("group_id"), mc.Query("group_name")); err != nil {
		return nil, err
	} else if set {
		var kept []*models.TransactionTag

		for _, t := range pool {
			if t.TagGroupId == gid {
				kept = append(kept, t)
			}
		}

		pool = kept
		filters["groupId"] = idString(gid)
	}

	if name := mc.Query("name"); name != "" {
		pool = refMatchByName(pool, name, refTagName)
		filters["name"] = name
	}

	rows := refTagViews(pool, groups)

	return map[string]any{"tags": rows, "count": len(rows), "filters": filters}, nil
}

func refHandleTagGet(mc *Ctx) (any, error) {
	tags, err := refLoadTags(mc)

	if err != nil {
		return nil, err
	}

	t, err := refResolveTag(tags, mc.Param("id"))

	if err != nil {
		return nil, err
	}

	groups, err := refLoadTagGroups(mc)

	if err != nil {
		return nil, err
	}

	used, err := refDB(mc).NewSession(mc.Web).Where("uid=? AND deleted=? AND tag_id=?", mc.Uid, false, t.TagId).Count(&models.TransactionTagIndex{})

	if err != nil {
		return nil, err
	}

	return map[string]any{"tag": refTagViews([]*models.TransactionTag{t}, groups)[0], "transactionCount": used}, nil
}

type refTagCreateReq struct {
	WriteOpts
	Name      string `json:"name"`
	GroupId   string `json:"group_id"`
	GroupName string `json:"group_name"`
}

func refHandleTagCreate(mc *Ctx) (any, error) {
	var req refTagCreateReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if err := refCheckIdemKey(req.IdempotencyKey); err != nil {
		return nil, err
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		name, err := refName("tag", req.Name)

		if err != nil {
			return nil, err
		}

		tags, err := refLoadTags(mc)

		if err != nil {
			return nil, err
		}

		groups, err := refLoadTagGroups(mc)

		if err != nil {
			return nil, err
		}

		gid, _, err := refTagGroupArg(groups, req.GroupId, req.GroupName)

		if err != nil {
			return nil, err
		}

		var warnings []string

		for _, t := range tags {
			if t.Name == name {
				return nil, Conflict("tag names are unique; use the existing tag (id "+idString(t.TagId)+")", "a tag named %q already exists", name).WithDetails(map[string]any{"id": idString(t.TagId)})
			}

			if strings.EqualFold(t.Name, name) {
				warnings = append(warnings, fmt.Sprintf("a tag %q differing only in case exists (id %s)", t.Name, idString(t.TagId)))
			}
		}

		preview := map[string]any{"action": "create", "name": name, "groupId": idString(gid)}

		return &Plan{Changes: map[string]int{"create": 1}, Count: 1, Preview: preview, Warnings: warnings, State: &models.TransactionTagCreateRequest{GroupId: gid, Name: name}}, nil
	}, func(p *Plan) (any, error) {
		if v, ok := refIdemGet(mc, req.IdempotencyKey); ok {
			return v, nil
		}

		res, err := mc.CallUpstream(api.TransactionTags.TagCreateHandler, "POST", nil, p.State)

		if err != nil {
			return nil, err
		}

		id, err := refCreatedId(res)

		if err != nil {
			return nil, err
		}

		if err := refJournalCreate(mc, "tag", []int64{id}, nil, "create tag "+idString(id)); err != nil {
			return nil, err
		}

		out, err := refTagResult(mc, id)

		if err != nil {
			return nil, err
		}

		refIdemPut(mc, req.IdempotencyKey, out)

		return out, nil
	})
}

func refTagResult(mc *Ctx, id int64) (any, error) {
	tags, err := refLoadTags(mc)

	if err != nil {
		return nil, err
	}

	groups, err := refLoadTagGroups(mc)

	if err != nil {
		return nil, err
	}

	for _, t := range tags {
		if t.TagId == id {
			return map[string]any{"tag": refTagViews([]*models.TransactionTag{t}, groups)[0]}, nil
		}
	}

	return map[string]any{"tag": map[string]any{"id": idString(id)}}, nil
}

// refTagBatchReq is POST /tags/batch — tags as ["name", …] or [{name}, …], all into one group
type refTagBatchReq struct {
	WriteOpts
	Tags         []json.RawMessage `json:"tags"`
	GroupId      string            `json:"group_id"`
	GroupName    string            `json:"group_name"`
	SkipExisting *bool             `json:"skip_existing"`
}

// refTagBatchNames parses the tags array (strings or {name} objects), de-duplicating in order
func refTagBatchNames(raw []json.RawMessage) ([]string, error) {
	var names []string
	seen := map[string]bool{}

	for i, r := range raw {
		var s string

		if err := json.Unmarshal(r, &s); err != nil {
			var obj struct {
				Name string `json:"name"`
			}

			dec := json.NewDecoder(bytes.NewReader(r))
			dec.DisallowUnknownFields()

			if err := dec.Decode(&obj); err != nil {
				return nil, Invalid("tags is an array of names, or of {name} objects", "tags[%d] is neither a name nor {name}", i)
			}

			s = obj.Name
		}

		name, err := refName("tag", s)

		if err != nil {
			return nil, Invalid(fmt.Sprintf("tags[%d] needs a non-empty name of at most 64 characters", i), "%s", err.(*Fail).Message)
		}

		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}

	if len(names) == 0 {
		return nil, Invalid("pass tags: [\"trip-2026\", …]", "no tags to create")
	}

	return names, nil
}

func refHandleTagBatch(mc *Ctx) (any, error) {
	var req refTagBatchReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	skip := req.SkipExisting == nil || *req.SkipExisting

	type state struct {
		Request models.TransactionTagCreateBatchRequest
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		names, err := refTagBatchNames(req.Tags)

		if err != nil {
			return nil, err
		}

		tags, err := refLoadTags(mc)

		if err != nil {
			return nil, err
		}

		groups, err := refLoadTagGroups(mc)

		if err != nil {
			return nil, err
		}

		gid, _, err := refTagGroupArg(groups, req.GroupId, req.GroupName)

		if err != nil {
			return nil, err
		}

		existing := map[string]*models.TransactionTag{}

		for _, t := range tags {
			existing[t.Name] = t
		}

		var creates, skips []string
		st := &state{Request: models.TransactionTagCreateBatchRequest{GroupId: gid, SkipExists: skip}}

		for _, n := range names {
			if t, ok := existing[n]; ok {
				if !skip {
					return nil, Conflict("pass skip_existing: true (the default) to leave existing tags alone", "a tag named %q already exists (id %s)", n, idString(t.TagId))
				}

				skips = append(skips, n)
				continue
			}

			creates = append(creates, n)
			st.Request.Tags = append(st.Request.Tags, &models.TransactionTagCreateRequest{GroupId: gid, Name: n})
		}

		if creates == nil {
			creates = []string{}
		}

		if skips == nil {
			skips = []string{}
		}

		preview := map[string]any{"create": creates, "skipExisting": skips, "groupId": idString(gid)}

		return &Plan{Changes: map[string]int{"create": len(creates), "unchanged": len(skips)}, Count: len(creates), Preview: preview, State: st}, nil
	}, func(p *Plan) (any, error) {
		st := p.State.(*state)

		if len(st.Request.Tags) == 0 {
			return map[string]any{"tags": []any{}, "created": 0}, nil
		}

		before, err := refLoadTags(mc)

		if err != nil {
			return nil, err
		}

		existed := map[int64]bool{}

		for _, t := range before {
			existed[t.TagId] = true
		}

		if _, err := mc.CallUpstream(api.TransactionTags.TagCreateBatchHandler, "POST", nil, st.Request); err != nil {
			return nil, err
		}

		after, err := refLoadTags(mc)

		if err != nil {
			return nil, err
		}

		groups, err := refLoadTagGroups(mc)

		if err != nil {
			return nil, err
		}

		var created []*models.TransactionTag
		var ids []int64

		for _, t := range after {
			if !existed[t.TagId] {
				created = append(created, t)
				ids = append(ids, t.TagId)
			}
		}

		if err := refJournalCreate(mc, "tag", ids, nil, fmt.Sprintf("create %d tags", len(ids))); err != nil {
			return nil, err
		}

		return map[string]any{"tags": refTagViews(created, groups), "created": len(created)}, nil
	})
}

// refTagFields are the editable fields of a tag (journal payloads)
type refTagFields struct {
	Id      string `json:"id"`
	Name    string `json:"name"`
	GroupId string `json:"groupId"`
}

func refTagFieldsOf(t *models.TransactionTag) refTagFields {
	return refTagFields{Id: idString(t.TagId), Name: t.Name, GroupId: idString(t.TagGroupId)}
}

func refApplyTagFields(mc *Ctx, f refTagFields) error {
	gid, err := strconv.ParseInt(f.GroupId, 10, 64)

	if err != nil {
		return Invalid("group ids are decimal strings", "group id %q is invalid", f.GroupId)
	}

	id, err := ResolveId("id", f.Id)

	if err != nil {
		return err
	}

	_, err = mc.CallUpstream(api.TransactionTags.TagModifyHandler, "POST", nil, &models.TransactionTagModifyRequest{Id: id, GroupId: gid, Name: f.Name})

	return err
}

type refTagPatchReq struct {
	WriteOpts
	Name      *string `json:"name"`
	GroupId   *string `json:"group_id"`
	GroupName *string `json:"group_name"`
	Hidden    *bool   `json:"hidden"`
}

func refHandleTagPatch(mc *Ctx) (any, error) {
	var req refTagPatchReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if req.Hidden != nil {
		return nil, Invalid("use POST /machine/v1/tags/:id/hide {hidden}", "hidden is set through the hide route")
	}

	ref := mc.Param("id")

	type state struct {
		Before, After refTagFields
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		tags, err := refLoadTags(mc)

		if err != nil {
			return nil, err
		}

		t, err := refResolveTag(tags, ref)

		if err != nil {
			return nil, err
		}

		groups, err := refLoadTagGroups(mc)

		if err != nil {
			return nil, err
		}

		before := refTagFieldsOf(t)
		after := before
		var changes []refFieldChange

		if req.Name != nil {
			name, err := refName("tag", *req.Name)

			if err != nil {
				return nil, err
			}

			if name != t.Name {
				for _, o := range tags {
					if o.TagId != t.TagId && o.Name == name {
						return nil, Conflict("tag names are unique; pick another name", "a tag named %q already exists (id %s)", name, idString(o.TagId))
					}
				}

				after.Name = name
				changes = append(changes, refFieldChange{Id: before.Id, Name: t.Name, Field: "name", From: t.Name, To: name})
			}
		}

		gidText, gname := "", ""

		if req.GroupId != nil {
			gidText = *req.GroupId
		}

		if req.GroupName != nil {
			gname = *req.GroupName
		}

		if gid, set, err := refTagGroupArg(groups, gidText, gname); err != nil {
			return nil, err
		} else if set && gid != t.TagGroupId {
			after.GroupId = idString(gid)
			changes = append(changes, refFieldChange{Id: before.Id, Name: t.Name, Field: "groupId", From: before.GroupId, To: after.GroupId})
		}

		if changes == nil {
			changes = []refFieldChange{}
		}

		plan := &Plan{Preview: changes, State: &state{Before: before, After: after}, Changes: map[string]int{"update": 0}}

		if len(changes) > 0 {
			plan.Changes["update"] = 1
			plan.Count = 1
		} else {
			plan.Changes["unchanged"] = 1
		}

		return plan, nil
	}, func(p *Plan) (any, error) {
		st := p.State.(*state)
		id, _ := strconv.ParseInt(st.Before.Id, 10, 64)

		if p.Count == 0 {
			out, err := refTagResult(mc, id)

			if m, ok := out.(map[string]any); ok {
				m["updated"] = 0
			}

			return out, err
		}

		if err := refApplyTagFields(mc, st.After); err != nil {
			return nil, err
		}

		if err := refJournalRestore(mc, "ref.restore_tag_fields", st.Before, st.After, "edit tag "+st.Before.Id); err != nil {
			return nil, err
		}

		out, err := refTagResult(mc, id)

		if m, ok := out.(map[string]any); ok {
			m["updated"] = 1
		}

		return out, err
	})
}

func refInvRestoreTag(mc *Ctx, payload, check json.RawMessage) error {
	var want refTagFields

	if err := json.Unmarshal(payload, &want); err != nil {
		return err
	}

	id, err := ResolveId("id", want.Id)

	if err != nil {
		return err
	}

	t, err := services.TransactionTags.GetTagByTagId(mc.Web, mc.Uid, id)

	if err != nil || t == nil {
		return Conflict("the tag was deleted since; nothing to restore", "tag %s no longer exists", want.Id)
	}

	if !refCheckMatches(refTagFieldsOf(t), check) {
		return Conflict("the tag was edited again since; nothing was changed", "tag %s changed since the write", want.Id)
	}

	if refCheckMatches(refTagFieldsOf(t), refMustJSON(want)) {
		return nil
	}

	return refApplyTagFields(mc, want)
}

func refTagSiblings(mc *Ctx, ref string) (int64, []refSibling, error) {
	tags, err := refLoadTags(mc)

	if err != nil {
		return 0, nil, err
	}

	t, err := refResolveTag(tags, ref)

	if err != nil {
		return 0, nil, err
	}

	var sibs []refSibling

	for _, o := range tags {
		if o.TagGroupId == t.TagGroupId {
			sibs = append(sibs, refSibling{Id: o.TagId, Name: o.Name, Order: o.DisplayOrder})
		}
	}

	return t.TagId, sibs, nil
}

func refTagHideTarget(mc *Ctx, ref string, hidden bool) (*refHideTarget, error) {
	tags, err := refLoadTags(mc)

	if err != nil {
		return nil, err
	}

	t, err := refResolveTag(tags, ref)

	if err != nil {
		return nil, err
	}

	return &refHideTarget{Id: t.TagId, Name: t.Name, Hidden: t.Hidden}, nil
}

func refTagDeleteTarget(mc *Ctx, ref string) (*refDeleteTarget, error) {
	tags, err := refLoadTags(mc)

	if err != nil {
		return nil, err
	}

	t, err := refResolveTag(tags, ref)

	if err != nil {
		return nil, err
	}

	used, err := refDB(mc).NewSession(mc.Web).Where("uid=? AND deleted=? AND tag_id=?", mc.Uid, false, t.TagId).Count(&models.TransactionTagIndex{})

	if err != nil {
		return nil, err
	}

	if used > 0 {
		return nil, Conflict("remove it from those transactions first (POST /machine/v1/transactions/tags/remove), or hide the tag", "%d transactions carry the tag %q; a tag in use cannot be deleted", used, t.Name).WithDetails(map[string]any{"transactions": used})
	}

	templates, err := refTemplatesUsing(mc, func(tt *models.TransactionTemplate) bool {
		for _, id := range tt.GetTagIds() {
			if id == t.TagId {
				return true
			}
		}

		return false
	})

	if err != nil {
		return nil, err
	}

	if len(templates) > 0 {
		return nil, Conflict("remove the tag from those templates first (PATCH /machine/v1/templates/:id)", "%d templates or active schedules carry the tag %q", len(templates), t.Name).WithDetails(map[string]any{"templates": templates})
	}

	return &refDeleteTarget{Id: t.TagId, Preview: map[string]any{"action": "delete", "id": idString(t.TagId), "name": t.Name}}, nil
}

func refTagRows(mc *Ctx, ids []int64) (map[int64]refRow, error) {
	var rows []*models.TransactionTag

	if err := refDB(mc).NewSession(mc.Web).Where("uid=?", mc.Uid).In("tag_id", ids).Find(&rows); err != nil {
		return nil, err
	}

	out := map[int64]refRow{}

	for _, r := range rows {
		out[r.TagId] = refRow{Id: r.TagId, Name: r.Name, Hidden: r.Hidden, Order: r.DisplayOrder, Deleted: r.Deleted}
	}

	return out, nil
}

var refTagKind = &refKind{
	Name: "tag",
	List: "GET /machine/v1/tags",
	Rows: refTagRows,
	Delete: func(mc *Ctx, id int64) error {
		_, err := mc.CallUpstream(api.TransactionTags.TagDeleteHandler, "POST", nil, map[string]any{"id": idString(id)})
		return err
	},
	Undelete: func(mc *Ctx, ids []int64) (int, error) {
		return refUndeleteGeneric(mc, &models.TransactionTag{}, "tag_id", ids)
	},
}

// --- tag groups

func refHandleTagGroupList(mc *Ctx) (any, error) {
	groups, err := refLoadTagGroups(mc)

	if err != nil {
		return nil, err
	}

	tags, err := refLoadTags(mc)

	if err != nil {
		return nil, err
	}

	count := map[int64]int{}

	for _, t := range tags {
		count[t.TagGroupId]++
	}

	filters := map[string]any{}

	if name := mc.Query("name"); name != "" {
		groups = refMatchByName(groups, name, refTagGroupName)
		filters["name"] = name
	}

	sorted := append([]*models.TransactionTagGroup(nil), groups...)

	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].DisplayOrder != sorted[j].DisplayOrder {
			return sorted[i].DisplayOrder < sorted[j].DisplayOrder
		}

		return sorted[i].TagGroupId < sorted[j].TagGroupId
	})

	rows := make([]*refTagGroupView, 0, len(sorted))

	for _, g := range sorted {
		rows = append(rows, &refTagGroupView{Id: idString(g.TagGroupId), Name: g.Name, DisplayOrder: g.DisplayOrder, TagCount: count[g.TagGroupId]})
	}

	return map[string]any{"tagGroups": rows, "count": len(rows), "ungroupedTagCount": count[0], "filters": filters}, nil
}

func refHandleTagGroupGet(mc *Ctx) (any, error) {
	groups, err := refLoadTagGroups(mc)

	if err != nil {
		return nil, err
	}

	g, err := refResolveTagGroup(groups, mc.Param("id"))

	if err != nil {
		return nil, err
	}

	tags, err := refLoadTags(mc)

	if err != nil {
		return nil, err
	}

	var in []*models.TransactionTag

	for _, t := range tags {
		if t.TagGroupId == g.TagGroupId {
			in = append(in, t)
		}
	}

	v := &refTagGroupView{Id: idString(g.TagGroupId), Name: g.Name, DisplayOrder: g.DisplayOrder, TagCount: len(in), Tags: refTagViews(in, groups)}

	return map[string]any{"tagGroup": v}, nil
}

type refTagGroupCreateReq struct {
	WriteOpts
	Name string `json:"name"`
}

func refHandleTagGroupCreate(mc *Ctx) (any, error) {
	var req refTagGroupCreateReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if err := refCheckIdemKey(req.IdempotencyKey); err != nil {
		return nil, err
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		name, err := refName("tag group", req.Name)

		if err != nil {
			return nil, err
		}

		groups, err := refLoadTagGroups(mc)

		if err != nil {
			return nil, err
		}

		var warnings []string

		for _, g := range groups {
			if strings.EqualFold(g.Name, name) {
				warnings = append(warnings, fmt.Sprintf("a tag group named %q already exists (id %s)", g.Name, idString(g.TagGroupId)))
			}
		}

		return &Plan{Changes: map[string]int{"create": 1}, Count: 1, Preview: map[string]any{"action": "create", "name": name}, Warnings: warnings, State: &models.TransactionTagGroupCreateRequest{Name: name}}, nil
	}, func(p *Plan) (any, error) {
		if v, ok := refIdemGet(mc, req.IdempotencyKey); ok {
			return v, nil
		}

		res, err := mc.CallUpstream(api.TransactionTagGroups.TagGroupCreateHandler, "POST", nil, p.State)

		if err != nil {
			return nil, err
		}

		id, err := refCreatedId(res)

		if err != nil {
			return nil, err
		}

		if err := refJournalCreate(mc, "tag_group", []int64{id}, nil, "create tag group "+idString(id)); err != nil {
			return nil, err
		}

		out := map[string]any{"tagGroup": map[string]any{"id": idString(id), "name": p.State.(*models.TransactionTagGroupCreateRequest).Name}}
		refIdemPut(mc, req.IdempotencyKey, out)

		return out, nil
	})
}

type refTagGroupFields struct {
	Id   string `json:"id"`
	Name string `json:"name"`
}

type refTagGroupPatchReq struct {
	WriteOpts
	Name *string `json:"name"`
}

func refHandleTagGroupPatch(mc *Ctx) (any, error) {
	var req refTagGroupPatchReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	ref := mc.Param("id")

	type state struct {
		Before, After refTagGroupFields
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		groups, err := refLoadTagGroups(mc)

		if err != nil {
			return nil, err
		}

		g, err := refResolveTagGroup(groups, ref)

		if err != nil {
			return nil, err
		}

		before := refTagGroupFields{Id: idString(g.TagGroupId), Name: g.Name}
		after := before
		changes := []refFieldChange{}

		if req.Name != nil {
			name, err := refName("tag group", *req.Name)

			if err != nil {
				return nil, err
			}

			if name != g.Name {
				after.Name = name
				changes = append(changes, refFieldChange{Id: before.Id, Name: g.Name, Field: "name", From: g.Name, To: name})
			}
		}

		plan := &Plan{Preview: changes, State: &state{Before: before, After: after}, Changes: map[string]int{"update": len(changes)}, Count: len(changes)}

		if len(changes) == 0 {
			plan.Changes["unchanged"] = 1
		}

		return plan, nil
	}, func(p *Plan) (any, error) {
		st := p.State.(*state)

		if p.Count == 0 {
			return map[string]any{"updated": 0, "tagGroup": st.Before}, nil
		}

		if err := refApplyTagGroupFields(mc, st.After); err != nil {
			return nil, err
		}

		if err := refJournalRestore(mc, "ref.restore_tag_group_fields", st.Before, st.After, "rename tag group "+st.Before.Id); err != nil {
			return nil, err
		}

		return map[string]any{"updated": 1, "tagGroup": st.After}, nil
	})
}

func refApplyTagGroupFields(mc *Ctx, f refTagGroupFields) error {
	id, err := ResolveId("id", f.Id)

	if err != nil {
		return err
	}

	_, err = mc.CallUpstream(api.TransactionTagGroups.TagGroupModifyHandler, "POST", nil, &models.TransactionTagGroupModifyRequest{Id: id, Name: f.Name})

	return err
}

func refInvRestoreTagGroup(mc *Ctx, payload, check json.RawMessage) error {
	var want refTagGroupFields

	if err := json.Unmarshal(payload, &want); err != nil {
		return err
	}

	id, err := ResolveId("id", want.Id)

	if err != nil {
		return err
	}

	g, err := services.TransactionTagGroups.GetTagGroupByTagGroupId(mc.Web, mc.Uid, id)

	if err != nil || g == nil {
		return Conflict("the tag group was deleted since; nothing to restore", "tag group %s no longer exists", want.Id)
	}

	current := refTagGroupFields{Id: want.Id, Name: g.Name}

	if !refCheckMatches(current, check) {
		return Conflict("the tag group was renamed again since; nothing was changed", "tag group %s changed since the write", want.Id)
	}

	if current.Name == want.Name {
		return nil
	}

	return refApplyTagGroupFields(mc, want)
}

func refTagGroupSiblings(mc *Ctx, ref string) (int64, []refSibling, error) {
	groups, err := refLoadTagGroups(mc)

	if err != nil {
		return 0, nil, err
	}

	g, err := refResolveTagGroup(groups, ref)

	if err != nil {
		return 0, nil, err
	}

	sibs := make([]refSibling, 0, len(groups))

	for _, o := range groups {
		sibs = append(sibs, refSibling{Id: o.TagGroupId, Name: o.Name, Order: o.DisplayOrder})
	}

	return g.TagGroupId, sibs, nil
}

func refTagGroupDeleteTarget(mc *Ctx, ref string) (*refDeleteTarget, error) {
	groups, err := refLoadTagGroups(mc)

	if err != nil {
		return nil, err
	}

	g, err := refResolveTagGroup(groups, ref)

	if err != nil {
		return nil, err
	}

	tags, err := refLoadTags(mc)

	if err != nil {
		return nil, err
	}

	var inside []string

	for _, t := range tags {
		if t.TagGroupId == g.TagGroupId {
			inside = append(inside, t.Name)
		}
	}

	if len(inside) > 0 {
		return nil, Conflict("move its tags to another group (PATCH /machine/v1/tags/:id {group_id}) or delete them first", "the tag group %q still holds %d tags", g.Name, len(inside)).WithDetails(map[string]any{"tags": inside})
	}

	return &refDeleteTarget{Id: g.TagGroupId, Preview: map[string]any{"action": "delete", "id": idString(g.TagGroupId), "name": g.Name}}, nil
}

func refTagGroupRows(mc *Ctx, ids []int64) (map[int64]refRow, error) {
	var rows []*models.TransactionTagGroup

	if err := refDB(mc).NewSession(mc.Web).Where("uid=?", mc.Uid).In("tag_group_id", ids).Find(&rows); err != nil {
		return nil, err
	}

	out := map[int64]refRow{}

	for _, r := range rows {
		out[r.TagGroupId] = refRow{Id: r.TagGroupId, Name: r.Name, Order: r.DisplayOrder, Deleted: r.Deleted}
	}

	return out, nil
}

var refTagGroupKind = &refKind{
	Name: "tag_group",
	List: "GET /machine/v1/tag-groups",
	Rows: refTagGroupRows,
	Delete: func(mc *Ctx, id int64) error {
		_, err := mc.CallUpstream(api.TransactionTagGroups.TagGroupDeleteHandler, "POST", nil, map[string]any{"id": idString(id)})
		return err
	},
	Undelete: func(mc *Ctx, ids []int64) (int, error) {
		return refUndeleteGeneric(mc, &models.TransactionTagGroup{}, "tag_group_id", ids)
	},
}

// ---------------------------------------------------------------------------------------------
// saved insights — explorer definitions (§10.8)
// ---------------------------------------------------------------------------------------------

type refInsightView struct {
	Id           string         `json:"id"`
	Name         string         `json:"name"`
	DisplayOrder int32          `json:"displayOrder"`
	Hidden       bool           `json:"hidden"`
	Data         map[string]any `json:"data,omitempty"`
}

func refLoadInsights(mc *Ctx) ([]*models.InsightsExplorer, error) {
	return services.InsightsExplorers.GetAllExplorationNamesByUid(mc.Web, mc.Uid)
}

func refInsightName(e *models.InsightsExplorer) string { return e.Name }
func refInsightId(e *models.InsightsExplorer) int64    { return e.ExplorerId }

func refResolveInsight(all []*models.InsightsExplorer, ref string) (*models.InsightsExplorer, error) {
	return refResolveOne("insight", ref, all, refInsightId, refInsightName, nil, "GET /machine/v1/insights?include_hidden=true lists them")
}

func refHandleInsightList(mc *Ctx) (any, error) {
	includeHidden, err := mc.QueryBool("include_hidden", false)

	if err != nil {
		return nil, err
	}

	all, err := refLoadInsights(mc)

	if err != nil {
		return nil, err
	}

	var pool []*models.InsightsExplorer

	for _, e := range all {
		if includeHidden || !e.Hidden {
			pool = append(pool, e)
		}
	}

	filters := map[string]any{"includeHidden": includeHidden}

	if name := mc.Query("name"); name != "" {
		pool = refMatchByName(pool, name, refInsightName)
		filters["name"] = name
	}

	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].DisplayOrder != pool[j].DisplayOrder {
			return pool[i].DisplayOrder < pool[j].DisplayOrder
		}

		return pool[i].ExplorerId < pool[j].ExplorerId
	})

	rows := make([]*refInsightView, 0, len(pool))

	for _, e := range pool {
		rows = append(rows, &refInsightView{Id: idString(e.ExplorerId), Name: e.Name, DisplayOrder: e.DisplayOrder, Hidden: e.Hidden})
	}

	return map[string]any{"insights": rows, "count": len(rows), "filters": filters, "note": "these are saved query definitions, not numbers; the numbers come from /machine/v1/analytics/*"}, nil
}

func refInsightFull(mc *Ctx, id int64) (*refInsightView, string, error) {
	e, err := services.InsightsExplorers.GetExplorationByExplorationId(mc.Web, mc.Uid, id)

	if err != nil {
		return nil, "", err
	}

	resp, err := e.ToInsightsExplorerInfoResponse()

	if err != nil {
		return nil, "", NewFail(CodeUpstreamError, "re-save the insight in the web UI", "the saved insight's definition is not valid JSON")
	}

	return &refInsightView{Id: idString(e.ExplorerId), Name: e.Name, DisplayOrder: e.DisplayOrder, Hidden: e.Hidden, Data: resp.Data}, e.Data, nil
}

func refHandleInsightGet(mc *Ctx) (any, error) {
	all, err := refLoadInsights(mc)

	if err != nil {
		return nil, err
	}

	e, err := refResolveInsight(all, mc.Param("id"))

	if err != nil {
		return nil, err
	}

	v, _, err := refInsightFull(mc, e.ExplorerId)

	if err != nil {
		return nil, err
	}

	return map[string]any{"insight": v}, nil
}

type refInsightCreateReq struct {
	WriteOpts
	Name       string         `json:"name"`
	Definition map[string]any `json:"definition"`
}

func refHandleInsightCreate(mc *Ctx) (any, error) {
	var req refInsightCreateReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if err := refCheckIdemKey(req.IdempotencyKey); err != nil {
		return nil, err
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		name, err := refName("insight", req.Name)

		if err != nil {
			return nil, err
		}

		if req.Definition == nil {
			return nil, Invalid("pass definition: the explorer's saved query object (GET /machine/v1/insights/:id shows one as data)", "definition is required")
		}

		body := &models.InsightsExplorerCreateRequest{Name: name, Data: req.Definition, ClientSessionId: req.IdempotencyKey}

		return &Plan{Changes: map[string]int{"create": 1}, Count: 1, Preview: map[string]any{"action": "create", "name": name, "definitionKeys": refSortedKeys(req.Definition)}, State: body}, nil
	}, func(p *Plan) (any, error) {
		if v, ok := refIdemGet(mc, req.IdempotencyKey); ok {
			return v, nil
		}

		res, err := mc.CallUpstream(api.InsightsExplorers.InsightsExplorerCreateHandler, "POST", nil, p.State)

		if err != nil {
			return nil, err
		}

		id, err := refCreatedId(res)

		if err != nil {
			return nil, err
		}

		if err := refJournalCreate(mc, "insight", []int64{id}, nil, "create insight "+idString(id)); err != nil {
			return nil, err
		}

		v, _, err := refInsightFull(mc, id)

		if err != nil {
			return nil, err
		}

		out := map[string]any{"insight": v}
		refIdemPut(mc, req.IdempotencyKey, out)

		return out, nil
	})
}

func refSortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))

	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

// refInsightFields is the stored, editable state of an insight (journal payloads); Data is the
// canonical JSON text upstream stores
type refInsightFields struct {
	Id   string `json:"id"`
	Name string `json:"name"`
	Data string `json:"data"`
}

type refInsightPatchReq struct {
	WriteOpts
	Name       *string        `json:"name"`
	Definition map[string]any `json:"definition"`
	Hidden     *bool          `json:"hidden"`
}

func refApplyInsightFields(mc *Ctx, f refInsightFields, hidden bool) error {
	id, err := ResolveId("id", f.Id)

	if err != nil {
		return err
	}

	var data map[string]any

	if err := json.Unmarshal([]byte(f.Data), &data); err != nil {
		return NewFail(CodeInternal, "read log/ezbookkeeping.log", "a stored insight definition is not JSON")
	}

	_, err = mc.CallUpstream(api.InsightsExplorers.InsightsExplorerModifyHandler, "POST", nil, &models.InsightsExplorerModifyRequest{Id: id, Name: f.Name, Data: data, Hidden: hidden})

	return err
}

func refHandleInsightPatch(mc *Ctx) (any, error) {
	var req refInsightPatchReq

	if err := mc.BindBody(&req); err != nil {
		return nil, err
	}

	if req.Hidden != nil {
		return nil, Invalid("use POST /machine/v1/insights/:id/hide {hidden}", "hidden is set through the hide route")
	}

	ref := mc.Param("id")

	type state struct {
		Before, After refInsightFields
		Hidden        bool
	}

	return RunWrite(mc, req.WriteOpts, func() (*Plan, error) {
		all, err := refLoadInsights(mc)

		if err != nil {
			return nil, err
		}

		e, err := refResolveInsight(all, ref)

		if err != nil {
			return nil, err
		}

		_, stored, err := refInsightFull(mc, e.ExplorerId)

		if err != nil {
			return nil, err
		}

		before := refInsightFields{Id: idString(e.ExplorerId), Name: e.Name, Data: stored}
		after := before
		changes := []refFieldChange{}

		if req.Name != nil {
			name, err := refName("insight", *req.Name)

			if err != nil {
				return nil, err
			}

			if name != e.Name {
				after.Name = name
				changes = append(changes, refFieldChange{Id: before.Id, Name: e.Name, Field: "name", From: e.Name, To: name})
			}
		}

		if req.Definition != nil {
			data, err := json.Marshal(req.Definition)

			if err != nil {
				return nil, Invalid("definition must be a JSON object", "definition cannot be encoded")
			}

			if string(data) != stored {
				after.Data = string(data)
				changes = append(changes, refFieldChange{Id: before.Id, Name: e.Name, Field: "definition", From: "(previous definition)", To: refSortedKeys(req.Definition)})
			}
		}

		plan := &Plan{Preview: changes, Fingerprinted: []any{changes, after}, State: &state{Before: before, After: after, Hidden: e.Hidden}, Changes: map[string]int{"update": len(changes)}}

		if len(changes) > 0 {
			plan.Count = 1
			plan.Changes["update"] = 1
		} else {
			plan.Changes["unchanged"] = 1
		}

		return plan, nil
	}, func(p *Plan) (any, error) {
		st := p.State.(*state)
		id, _ := strconv.ParseInt(st.Before.Id, 10, 64)

		if p.Count > 0 {
			if err := refApplyInsightFields(mc, st.After, st.Hidden); err != nil {
				return nil, err
			}

			if err := refJournalRestore(mc, "ref.restore_insight", st.Before, st.After, "edit insight "+st.Before.Id); err != nil {
				return nil, err
			}
		}

		v, _, err := refInsightFull(mc, id)

		if err != nil {
			return nil, err
		}

		return map[string]any{"insight": v, "updated": p.Count}, nil
	})
}

func refInvRestoreInsight(mc *Ctx, payload, check json.RawMessage) error {
	var want refInsightFields

	if err := json.Unmarshal(payload, &want); err != nil {
		return err
	}

	id, err := ResolveId("id", want.Id)

	if err != nil {
		return err
	}

	e, err := services.InsightsExplorers.GetExplorationByExplorationId(mc.Web, mc.Uid, id)

	if err != nil || e == nil {
		return Conflict("the insight was deleted since; nothing to restore", "insight %s no longer exists", want.Id)
	}

	current := refInsightFields{Id: want.Id, Name: e.Name, Data: e.Data}

	if !refCheckMatches(current, check) {
		return Conflict("the insight was edited again since; nothing was changed", "insight %s changed since the write", want.Id)
	}

	if current == want {
		return nil
	}

	return refApplyInsightFields(mc, want, e.Hidden)
}

func refInsightSiblings(mc *Ctx, ref string) (int64, []refSibling, error) {
	all, err := refLoadInsights(mc)

	if err != nil {
		return 0, nil, err
	}

	e, err := refResolveInsight(all, ref)

	if err != nil {
		return 0, nil, err
	}

	sibs := make([]refSibling, 0, len(all))

	for _, o := range all {
		sibs = append(sibs, refSibling{Id: o.ExplorerId, Name: o.Name, Order: o.DisplayOrder})
	}

	return e.ExplorerId, sibs, nil
}

func refInsightHideTarget(mc *Ctx, ref string, hidden bool) (*refHideTarget, error) {
	all, err := refLoadInsights(mc)

	if err != nil {
		return nil, err
	}

	e, err := refResolveInsight(all, ref)

	if err != nil {
		return nil, err
	}

	return &refHideTarget{Id: e.ExplorerId, Name: e.Name, Hidden: e.Hidden}, nil
}

func refInsightDeleteTarget(mc *Ctx, ref string) (*refDeleteTarget, error) {
	all, err := refLoadInsights(mc)

	if err != nil {
		return nil, err
	}

	e, err := refResolveInsight(all, ref)

	if err != nil {
		return nil, err
	}

	return &refDeleteTarget{Id: e.ExplorerId, Preview: map[string]any{"action": "delete", "id": idString(e.ExplorerId), "name": e.Name}}, nil
}

func refInsightRows(mc *Ctx, ids []int64) (map[int64]refRow, error) {
	var rows []*models.InsightsExplorer

	if err := refDB(mc).NewSession(mc.Web).Cols("explorer_id", "uid", "deleted", "name", "display_order", "hidden").Where("uid=?", mc.Uid).In("explorer_id", ids).Find(&rows); err != nil {
		return nil, err
	}

	out := map[int64]refRow{}

	for _, r := range rows {
		out[r.ExplorerId] = refRow{Id: r.ExplorerId, Name: r.Name, Hidden: r.Hidden, Order: r.DisplayOrder, Deleted: r.Deleted}
	}

	return out, nil
}

var refInsightKind = &refKind{
	Name: "insight",
	List: "GET /machine/v1/insights",
	Rows: refInsightRows,
	Delete: func(mc *Ctx, id int64) error {
		_, err := mc.CallUpstream(api.InsightsExplorers.InsightsExplorerDeleteHandler, "POST", nil, map[string]any{"id": idString(id)})
		return err
	},
	Undelete: func(mc *Ctx, ids []int64) (int, error) {
		return refUndeleteGeneric(mc, &models.InsightsExplorer{}, "explorer_id", ids)
	},
}

// ---------------------------------------------------------------------------------------------
// templates and scheduled transactions (§10.6)
// ---------------------------------------------------------------------------------------------

var refTxnTypeNames = map[models.TransactionType]string{
	models.TRANSACTION_TYPE_MODIFY_BALANCE: "balance_modification",
	models.TRANSACTION_TYPE_INCOME:         "income",
	models.TRANSACTION_TYPE_EXPENSE:        "expense",
	models.TRANSACTION_TYPE_TRANSFER:       "transfer",
}

// refTemplateTxnType parses a template's transaction type (templates never hold a balance modification)
func refTemplateTxnType(v string) (models.TransactionType, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "income", "2":
		return models.TRANSACTION_TYPE_INCOME, nil
	case "expense", "3":
		return models.TRANSACTION_TYPE_EXPENSE, nil
	case "transfer", "4":
		return models.TRANSACTION_TYPE_TRANSFER, nil
	case "balance_modification", "balance-modification", "1":
		return 0, Invalid("a template is income, expense or transfer; an opening balance comes from account creation", "a template cannot be a balance modification")
	}

	return 0, Invalid("type is income, expense or transfer", "unknown transaction type %q", v)
}

var refFreqNames = map[models.TransactionScheduleFrequencyType]string{
	models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DISABLED:     "disabled",
	models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_WEEKLY:       "weekly",
	models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_MONTHLY:      "monthly",
	models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DAILY:        "daily",
	models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_YEARLY:       "yearly",
	models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_EVERY_N_DAYS: "every_n_days",
}

// refFreqType parses daily|weekly|monthly|yearly|every_n_days|disabled
func refFreqType(v string) (models.TransactionScheduleFrequencyType, error) {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(v), "-", "_")) {
	case "disabled", "paused", "none", "0":
		return models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DISABLED, nil
	case "weekly", "1":
		return models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_WEEKLY, nil
	case "monthly", "2":
		return models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_MONTHLY, nil
	case "daily", "3":
		return models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DAILY, nil
	case "yearly", "4":
		return models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_YEARLY, nil
	case "every_n_days", "every_n_day", "n_days", "5":
		return models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_EVERY_N_DAYS, nil
	}

	return 0, Invalid("frequency is daily, weekly, monthly, yearly, every_n_days or disabled (paused)", "unknown frequency %q", v)
}

var refWeekdays = map[string]int{
	"sun": 0, "sunday": 0, "mon": 1, "monday": 1, "tue": 2, "tues": 2, "tuesday": 2, "wed": 3, "wednesday": 3,
	"thu": 4, "thur": 4, "thurs": 4, "thursday": 4, "fri": 5, "friday": 5, "sat": 6, "saturday": 6,
}

var refMonthDayRe = regexp.MustCompile(`^(\d{1,2})-(\d{1,2})$`)

// refFrequencyTokens flattens frequency_value (a number, "1,15", or an array of numbers/strings)
func refFrequencyTokens(raw json.RawMessage) ([]string, error) {
	s := strings.TrimSpace(string(raw))

	if s == "" || s == "null" {
		return nil, nil
	}

	var items []json.RawMessage

	if strings.HasPrefix(s, "[") {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, Invalid("frequency_value is a number, a comma-separated string, or an array", "frequency_value is malformed")
		}
	} else {
		items = []json.RawMessage{raw}
	}

	var out []string

	for _, it := range items {
		var f refFlex

		if err := json.Unmarshal(it, &f); err != nil {
			return nil, Invalid("frequency_value items are numbers or strings", "frequency_value holds %s", string(it))
		}

		for _, part := range strings.Split(string(f), ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}

	return out, nil
}

// refDaysInMonthMax is the most days a month can have (Feb counts 29: a Feb-29 yearly schedule is
// upstream's to skip in common years)
func refDaysInMonthMax(m int) int {
	switch m {
	case 2:
		return 29
	case 4, 6, 9, 11:
		return 30
	}

	return 31
}

// refParseFrequency turns frequency + frequency_value into upstream's stored frequency string —
// sorted, de-duplicated integers, exactly what TransactionTemplatesApi.getOrderedFrequencyValues
// produces — with the calendar semantics of the cron (services.CreateScheduledTransactions):
// weekly 0-6 (Sunday 0), monthly 1-31 or -1…-31 counted from the month's end, yearly MMDD,
// every_n_days a single N ≥ 1 counted from start, daily "0".
func refParseFrequency(ft models.TransactionScheduleFrequencyType, raw json.RawMessage) (string, []string, error) {
	tokens, err := refFrequencyTokens(raw)

	if err != nil {
		return "", nil, err
	}

	var values []int
	var warnings []string
	seen := map[int]bool{}
	add := func(v int) {
		if !seen[v] {
			seen[v] = true
			values = append(values, v)
		}
	}

	switch ft {
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DISABLED:
		if len(tokens) > 0 {
			return "", nil, Invalid("a paused (disabled) schedule takes no frequency_value", "frequency_value given for a disabled schedule")
		}

		return "", nil, nil
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DAILY:
		for _, t := range tokens {
			if t != "0" {
				return "", nil, Invalid("daily takes no frequency_value", "frequency_value %q does not apply to daily", t)
			}
		}

		return "0", nil, nil
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_WEEKLY:
		for _, t := range tokens {
			if n, ok := refWeekdays[strings.ToLower(t)]; ok {
				add(n)
				continue
			}

			n, err := strconv.Atoi(t)

			if err != nil || n < 0 || n > 6 {
				return "", nil, Invalid("weekly takes weekdays: sun..sat, or 0-6 with Sunday 0", "%q is not a weekday", t)
			}

			add(n)
		}
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_MONTHLY:
		for _, t := range tokens {
			n, err := strconv.Atoi(t)

			if err != nil || n == 0 || n > 31 || n < -31 {
				return "", nil, Invalid("monthly takes days of the month: 1-31, or -1 for the last day (-2 the day before, …)", "%q is not a day of the month", t)
			}

			if n > 28 {
				warnings = append(warnings, fmt.Sprintf("day %d does not exist in every month; the app SKIPS months without it (use -1 for \"the last day\")", n))
			}

			add(n)
		}
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_YEARLY:
		for _, t := range tokens {
			m, d := 0, 0

			if mm := refMonthDayRe.FindStringSubmatch(t); mm != nil {
				m, _ = strconv.Atoi(mm[1])
				d, _ = strconv.Atoi(mm[2])
			} else if n, err := strconv.Atoi(t); err == nil {
				m, d = n/100, n%100
			} else {
				return "", nil, Invalid("yearly takes MM-DD dates (or MMDD numbers like 1225)", "%q is not a month and day", t)
			}

			if m < 1 || m > 12 || d < 1 || d > refDaysInMonthMax(m) {
				return "", nil, Invalid("yearly takes real MM-DD dates", "%q is not a real month and day", t)
			}

			if m == 2 && d == 29 {
				warnings = append(warnings, "02-29 only occurs in leap years; the app skips the other years")
			}

			add(m*100 + d)
		}
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_EVERY_N_DAYS:
		if len(tokens) != 1 {
			return "", nil, Invalid("every_n_days takes exactly one frequency_value: the interval N in days", "every_n_days needs one interval, got %d values", len(tokens))
		}

		n, err := strconv.Atoi(tokens[0])

		if err != nil || n < 1 || n > 3660 {
			return "", nil, Invalid("the interval is a whole number of days, 1-3660", "%q is not an interval in days", tokens[0])
		}

		add(n)
	default:
		return "", nil, Invalid("frequency is daily, weekly, monthly, yearly or every_n_days", "unknown frequency")
	}

	if len(values) == 0 {
		return "", nil, Invalid("pass frequency_value: weekdays for weekly, days for monthly, MM-DD for yearly, N for every_n_days", "%s needs a frequency_value", refFreqNames[ft])
	}

	sort.Ints(values)
	parts := make([]string, len(values))

	for i, v := range values {
		parts[i] = strconv.Itoa(v)
	}

	return strings.Join(parts, ","), warnings, nil
}

// refDescribeFrequency renders a stored frequency for a human ("monthly on day 1, 15")
func refDescribeFrequency(ft models.TransactionScheduleFrequencyType, value string) string {
	switch ft {
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DISABLED:
		return "paused"
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DAILY:
		return "daily"
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_WEEKLY:
		names := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
		var out []string

		for _, p := range strings.Split(value, ",") {
			if n, err := strconv.Atoi(p); err == nil && n >= 0 && n <= 6 {
				out = append(out, names[n])
			}
		}

		return "weekly on " + strings.Join(out, ", ")
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_MONTHLY:
		var out []string

		for _, p := range strings.Split(value, ",") {
			if n, err := strconv.Atoi(p); err == nil {
				if n == -1 {
					out = append(out, "the last day")
				} else if n < 0 {
					out = append(out, fmt.Sprintf("%d days before the end", -n-1))
				} else {
					out = append(out, "day "+p)
				}
			}
		}

		return "monthly on " + strings.Join(out, ", ")
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_YEARLY:
		var out []string

		for _, p := range strings.Split(value, ",") {
			if n, err := strconv.Atoi(p); err == nil {
				out = append(out, fmt.Sprintf("%02d-%02d", n/100, n%100))
			}
		}

		return "yearly on " + strings.Join(out, ", ")
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_EVERY_N_DAYS:
		return "every " + value + " days"
	}

	return "unknown"
}

// refScheduleFiresAt reports whether upstream's cron job (services.Transactions.
// CreateScheduledTransactions) would create a transaction from template t at txUnix, the unix time of
// one day's scheduled minute (UTC midnight + ScheduledAt minutes). It is the cron's per-day predicate,
// line for line, so /schedules/upcoming and the cron cannot disagree about the 31st of a 30-day month.
// NOTE for maintainers: upstream keeps this arithmetic inline in the cron; when upstream is next
// touched, the cron should call this function instead (apis.mdx §10.6 "computed once").
func refScheduleFiresAt(t *models.TransactionTemplate, txUnix int64) bool {
	switch t.ScheduledFrequencyType {
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_WEEKLY, models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_MONTHLY,
		models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DAILY, models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_YEARLY,
		models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_EVERY_N_DAYS:
	default:
		return false
	}

	if t.ScheduledFrequency == "" {
		return false
	}

	if t.Type != models.TRANSACTION_TYPE_INCOME && t.Type != models.TRANSACTION_TYPE_EXPENSE && t.Type != models.TRANSACTION_TYPE_TRANSFER {
		return false
	}

	var values []int64

	for _, p := range strings.Split(t.ScheduledFrequency, ",") {
		n, err := strconv.ParseInt(p, 10, 64)

		if err != nil {
			return false // the cron skips a template whose frequency does not parse
		}

		values = append(values, n)
	}

	tz := time.FixedZone("Template Timezone", int(t.ScheduledTimezoneUtcOffset)*60)
	txTime := time.Unix(txUnix, 0).In(tz)

	if t.ScheduledFrequencyType == models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_MONTHLY {
		maxDay := int64(time.Date(txTime.Year(), txTime.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day())

		for i := range values {
			if values[i] < 0 {
				values[i] = maxDay + values[i] + 1
			}
		}
	}

	set := map[int64]bool{}

	for _, v := range values {
		set[v] = true
	}

	switch t.ScheduledFrequencyType {
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_WEEKLY:
		if !set[int64(txTime.Weekday())] {
			return false
		}
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_MONTHLY:
		if !set[int64(txTime.Day())] {
			return false
		}
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_YEARLY:
		if !set[int64(txTime.Month())*100+int64(txTime.Day())] {
			return false
		}
	case models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_EVERY_N_DAYS:
		if t.ScheduledStartTime == nil || len(values) != 1 || values[0] <= 0 {
			return false
		}

		start := time.Unix(*t.ScheduledStartTime, 0).In(tz)
		startDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, tz)
		txDay := time.Date(txTime.Year(), txTime.Month(), txTime.Day(), 0, 0, 0, 0, tz)
		daysDiff := int(txDay.Sub(startDay).Hours() / 24)

		if daysDiff < 0 || int64(daysDiff)%values[0] != 0 {
			return false
		}
	}

	if t.ScheduledStartTime != nil && *t.ScheduledStartTime > txUnix {
		return false
	}

	if t.ScheduledEndTime != nil && *t.ScheduledEndTime < txUnix {
		return false
	}

	return true
}

// refScheduleOccurrences lists the unix instants in [fromUnix, toUnix] at which the cron would create
// a transaction from t, at most max of them (0 = no cap)
func refScheduleOccurrences(t *models.TransactionTemplate, fromUnix, toUnix int64, max int) []int64 {
	var out []int64
	first := time.Unix(fromUnix, 0).UTC()
	day := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, time.UTC).Unix()

	for ; day <= toUnix; day += 86400 {
		tx := day + int64(t.ScheduledAt)*60

		if tx < fromUnix || tx > toUnix {
			continue
		}

		if refScheduleFiresAt(t, tx) {
			out = append(out, tx)

			if max > 0 && len(out) >= max {
				break
			}
		}
	}

	return out
}

// refLookups are the bound user's accounts, categories and tags, for name resolution and views
type refLookups struct {
	Accounts   []*models.Account
	AccountBy  map[int64]*models.Account
	Categories []*models.TransactionCategory
	CategoryBy map[int64]*models.TransactionCategory
	Tags       []*models.TransactionTag
	TagBy      map[int64]*models.TransactionTag
}

func refLoadLookups(mc *Ctx) (*refLookups, error) {
	accounts, err := refLoadAccounts(mc)

	if err != nil {
		return nil, err
	}

	cats, err := refLoadCategories(mc)

	if err != nil {
		return nil, err
	}

	tags, err := refLoadTags(mc)

	if err != nil {
		return nil, err
	}

	lk := &refLookups{Accounts: accounts, AccountBy: map[int64]*models.Account{}, Categories: cats, CategoryBy: map[int64]*models.TransactionCategory{}, Tags: tags, TagBy: map[int64]*models.TransactionTag{}}

	for _, a := range accounts {
		lk.AccountBy[a.AccountId] = a
	}

	for _, c := range cats {
		lk.CategoryBy[c.CategoryId] = c
	}

	for _, t := range tags {
		lk.TagBy[t.TagId] = t
	}

	return lk, nil
}

// refTemplateSpec is the full editable state of a template — the planning form, and the journal's
// before/after for PATCH
type refTemplateSpec struct {
	Id                   string   `json:"id,omitempty"`
	Kind                 string   `json:"kind"`
	Name                 string   `json:"name"`
	Type                 string   `json:"type"`
	CategoryId           string   `json:"categoryId"`
	AccountId            string   `json:"accountId"`
	DestinationAccountId string   `json:"destinationAccountId"`
	Amount               int64    `json:"amount"`
	DestinationAmount    int64    `json:"destinationAmount"`
	HideAmount           bool     `json:"hideAmount"`
	TagIds               []string `json:"tagIds"`
	Comment              string   `json:"comment"`
	Frequency            string   `json:"frequency,omitempty"`
	FrequencyValue       string   `json:"frequencyValue,omitempty"`
	Start                string   `json:"start,omitempty"`
	End                  string   `json:"end,omitempty"`
	UtcOffset            int16    `json:"utcOffset"`
}

func refTemplateKind(tt models.TransactionTemplateType) string {
	if tt == models.TRANSACTION_TEMPLATE_TYPE_SCHEDULE {
		return "scheduled"
	}

	return "normal"
}

// refTemplateSpecOf reads a stored template into its spec
func refTemplateSpecOf(t *models.TransactionTemplate) refTemplateSpec {
	s := refTemplateSpec{
		Id: idString(t.TemplateId), Kind: refTemplateKind(t.TemplateType), Name: t.Name, Type: refTxnTypeNames[t.Type],
		CategoryId: idString(t.CategoryId), AccountId: idString(t.AccountId), DestinationAccountId: idString(t.RelatedAccountId),
		Amount: t.Amount, DestinationAmount: t.RelatedAccountAmount, HideAmount: t.HideAmount, TagIds: []string{}, Comment: t.Comment,
	}

	if t.TagIds != "" {
		s.TagIds = strings.Split(t.TagIds, ",")
	}

	if t.TemplateType == models.TRANSACTION_TEMPLATE_TYPE_SCHEDULE {
		tz := time.FixedZone("Template Timezone", int(t.ScheduledTimezoneUtcOffset)*60)
		s.Frequency = refFreqNames[t.ScheduledFrequencyType]
		s.FrequencyValue = t.ScheduledFrequency
		s.UtcOffset = t.ScheduledTimezoneUtcOffset

		if t.ScheduledStartTime != nil {
			s.Start = time.Unix(*t.ScheduledStartTime, 0).In(tz).Format("2006-01-02")
		}

		if t.ScheduledEndTime != nil {
			s.End = time.Unix(*t.ScheduledEndTime, 0).In(tz).Format("2006-01-02")
		}
	}

	return s
}

// refTemplateBody is the request of POST /templates and PATCH /templates/:id. On PATCH every field
// is optional and absent means unchanged; "end": "" removes the end date.
type refTemplateBody struct {
	WriteOpts
	Kind                   string          `json:"kind"`
	Name                   *string         `json:"name"`
	Type                   *string         `json:"type"`
	CategoryId             *string         `json:"category_id"`
	CategoryName           *string         `json:"category_name"`
	AccountId              *string         `json:"account_id"`
	AccountName            *string         `json:"account_name"`
	DestinationAccountId   *string         `json:"destination_account_id"`
	DestinationAccountName *string         `json:"destination_account_name"`
	Amount                 *json.Number    `json:"amount"`
	DestinationAmount      *json.Number    `json:"destination_amount"`
	HideAmount             *bool           `json:"hide_amount"`
	TagIds                 *[]string       `json:"tag_ids"`
	TagNames               *[]string       `json:"tag_names"`
	Comment                *string         `json:"comment"`
	Frequency              *string         `json:"frequency"`
	FrequencyValue         json.RawMessage `json:"frequency_value"`
	Start                  *string         `json:"start"`
	End                    *string         `json:"end"`
	Timezone               *string         `json:"timezone"`
	Time                   *string         `json:"time"`
	Hidden                 *bool           `json:"hidden"`
}

func refStr(p *string) string {
	if p == nil {
		return ""
	}

	return *p
}

// refApplyTemplateBody applies a request body to a spec (create starts from an empty spec), then
// validates the result against the books the way upstream's isTemplateValid will. It returns the
// warnings the caller should read before confirming.
func refApplyTemplateBody(spec *refTemplateSpec, body *refTemplateBody, lk *refLookups, loc *time.Location, now time.Time, create bool) ([]string, error) {
	var warnings []string

	if body.Time != nil {
		return nil, Invalid("scheduled transactions are created at 00:00 in the schedule's timezone; upstream has no time-of-day for them (drop \"time\")", "time is not a template field")
	}

	if body.Hidden != nil {
		return nil, Invalid("use POST /machine/v1/templates/:id/hide {hidden}", "hidden is set through the hide route")
	}

	if create {
		kind := strings.ToLower(strings.TrimSpace(body.Kind))

		if kind == "" && body.Frequency != nil {
			kind = "scheduled"
		}

		switch kind {
		case "", "normal", "template":
			spec.Kind = "normal"
		case "scheduled", "schedule":
			spec.Kind = "scheduled"
		default:
			return nil, Invalid("kind is normal or scheduled", "unknown kind %q", body.Kind)
		}
	} else if body.Kind != "" && strings.ToLower(strings.TrimSpace(body.Kind)) != spec.Kind && !(spec.Kind == "scheduled" && strings.EqualFold(body.Kind, "schedule")) {
		return nil, Invalid("a template cannot turn into a schedule or back; create a new one and delete this", "kind cannot change from %s", spec.Kind)
	}

	if spec.Kind == "normal" && (body.Frequency != nil || len(body.FrequencyValue) > 0 || body.Start != nil || body.End != nil || body.Timezone != nil) {
		return nil, Invalid("frequency, frequency_value, start, end and timezone belong to kind: scheduled", "schedule fields were given for a normal template")
	}

	if body.Name != nil || create {
		name, err := refName("template", refStr(body.Name))

		if err != nil {
			return nil, err
		}

		spec.Name = name
	}

	if body.Type != nil {
		t, err := refTemplateTxnType(*body.Type)

		if err != nil {
			return nil, err
		}

		spec.Type = refTxnTypeNames[t]
	} else if create {
		return nil, Invalid("pass type: income, expense or transfer", "type is required")
	}

	typ, _ := refTemplateTxnType(spec.Type)

	// account
	if ref, err := refRefArg("account", refStr(body.AccountId), refStr(body.AccountName)); err != nil {
		return nil, err
	} else if ref != "" {
		a, err := refResolveAccount(lk.Accounts, ref)

		if err != nil {
			return nil, err
		}

		spec.AccountId = idString(a.AccountId)
	} else if create {
		return nil, Invalid("pass account_id or account_name", "the account is required")
	}

	// destination account
	if ref, err := refRefArg("destination_account", refStr(body.DestinationAccountId), refStr(body.DestinationAccountName)); err != nil {
		return nil, err
	} else if ref != "" {
		if typ != models.TRANSACTION_TYPE_TRANSFER {
			return nil, Invalid("only a transfer has a destination account", "destination_account given for a %s", spec.Type)
		}

		a, err := refResolveAccount(lk.Accounts, ref)

		if err != nil {
			return nil, err
		}

		spec.DestinationAccountId = idString(a.AccountId)
	}

	if typ != models.TRANSACTION_TYPE_TRANSFER {
		spec.DestinationAccountId = "0"
		spec.DestinationAmount = 0
	}

	// category
	if ref, err := refRefArg("category", refStr(body.CategoryId), refStr(body.CategoryName)); err != nil {
		return nil, err
	} else if ref != "" {
		c, err := refResolveCategory(nil, lk.Categories, ref)

		if err != nil {
			return nil, err
		}

		spec.CategoryId = idString(c.CategoryId)
	} else if create {
		return nil, Invalid("pass category_id or category_name (a secondary "+spec.Type+" category)", "the category is required")
	}

	// amounts
	if body.Amount != nil {
		n, err := AmountArg("amount", *body.Amount)

		if err != nil {
			return nil, err
		}

		if n < 0 {
			return nil, Invalid("amounts are positive; the type (income, expense, transfer) carries the direction", "amount %d is negative", n)
		}

		spec.Amount = n
	} else if create {
		return nil, Invalid("pass amount in integer hundredths (12.50 is 1250)", "amount is required")
	}

	destAmountGiven := false

	if body.DestinationAmount != nil {
		if typ != models.TRANSACTION_TYPE_TRANSFER {
			return nil, Invalid("only a transfer has a destination amount", "destination_amount given for a %s", spec.Type)
		}

		n, err := AmountArg("destination_amount", *body.DestinationAmount)

		if err != nil {
			return nil, err
		}

		if n < 0 {
			return nil, Invalid("amounts are positive", "destination_amount %d is negative", n)
		}

		spec.DestinationAmount = n
		destAmountGiven = true
	}

	if body.HideAmount != nil {
		spec.HideAmount = *body.HideAmount
	}

	if body.Comment != nil {
		c, err := refComment(*body.Comment)

		if err != nil {
			return nil, err
		}

		spec.Comment = c
	}

	// tags
	if body.TagIds != nil || body.TagNames != nil {
		var ids []string
		seen := map[string]bool{}

		if body.TagIds != nil {
			for _, s := range *body.TagIds {
				t, err := refResolveTag(lk.Tags, "id:"+strings.TrimSpace(s))

				if err != nil {
					return nil, err
				}

				if id := idString(t.TagId); !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}

		if body.TagNames != nil {
			for _, s := range *body.TagNames {
				t, err := refResolveTag(lk.Tags, "name:"+s)

				if err != nil {
					return nil, err
				}

				if id := idString(t.TagId); !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}

		if ids == nil {
			ids = []string{}
		}

		spec.TagIds = ids
	}

	// schedule
	if spec.Kind == "scheduled" {
		if body.Timezone != nil {
			tzLoc, err := time.LoadLocation(strings.TrimSpace(*body.Timezone))

			if err != nil {
				return nil, Invalid("timezone is an IANA name like America/Los_Angeles", "unknown timezone %q", *body.Timezone)
			}

			spec.UtcOffset = UTCOffsetMinutes(now, tzLoc)
			warnings = append(warnings, "upstream stores a schedule's timezone as a fixed UTC offset ("+strconv.Itoa(int(spec.UtcOffset))+" minutes); it does not follow daylight-saving changes")
		} else if create {
			spec.UtcOffset = UTCOffsetMinutes(now, loc)
		}

		if body.Frequency != nil || len(body.FrequencyValue) > 0 || create {
			fname := spec.Frequency

			if body.Frequency != nil {
				fname = *body.Frequency
			}

			if fname == "" {
				return nil, Invalid("pass frequency: daily, weekly, monthly, yearly or every_n_days", "a scheduled template needs a frequency")
			}

			ft, err := refFreqType(fname)

			if err != nil {
				return nil, err
			}

			raw := body.FrequencyValue

			if len(raw) == 0 && body.Frequency == nil {
				raw = refMustJSON(spec.FrequencyValue)
			}

			value, w, err := refParseFrequency(ft, raw)

			if err != nil {
				return nil, err
			}

			spec.Frequency = refFreqNames[ft]
			spec.FrequencyValue = value
			warnings = append(warnings, w...)
		}

		if body.Start != nil {
			if strings.TrimSpace(*body.Start) == "" {
				spec.Start = ""
			} else {
				d, err := ParseDate("start", *body.Start, time.UTC)

				if err != nil {
					return nil, err
				}

				spec.Start = d.Format("2006-01-02")
			}
		}

		if body.End != nil {
			if strings.TrimSpace(*body.End) == "" {
				spec.End = ""
			} else {
				d, err := ParseDate("end", *body.End, time.UTC)

				if err != nil {
					return nil, err
				}

				spec.End = d.Format("2006-01-02")
			}
		}

		if spec.Frequency == "every_n_days" && spec.Start == "" {
			return nil, Invalid("every_n_days counts from start: pass start (YYYY-MM-DD)", "start is required for every_n_days")
		}

		if spec.Start != "" && spec.End != "" && spec.End < spec.Start {
			return nil, Invalid("end must be on or after start", "end %s is before start %s", spec.End, spec.Start)
		}

		if spec.End != "" && spec.End < now.In(loc).Format("2006-01-02") {
			warnings = append(warnings, "the end date is in the past; this schedule will not create anything")
		}

		if spec.Frequency == "disabled" {
			warnings = append(warnings, "the schedule is PAUSED (frequency disabled); it creates nothing until a frequency is set")
		}
	}

	w, err := refValidateTemplateSpec(spec, lk, destAmountGiven)

	if err != nil {
		return nil, err
	}

	return append(warnings, w...), nil
}

// refValidateTemplateSpec checks a spec against the books the way upstream's isTemplateValid does,
// with a hint that names the fix
func refValidateTemplateSpec(spec *refTemplateSpec, lk *refLookups, destAmountGiven bool) ([]string, error) {
	var warnings []string
	typ, err := refTemplateTxnType(spec.Type)

	if err != nil {
		return nil, err
	}

	accountId, _ := strconv.ParseInt(spec.AccountId, 10, 64)
	src, ok := lk.AccountBy[accountId]

	if !ok {
		return nil, NotFound("GET /machine/v1/accounts lists them", "the template's account %s no longer exists", spec.AccountId)
	}

	if err := refUsableAccount(src, lk.Accounts, "account"); err != nil {
		return nil, err
	}

	if typ == models.TRANSACTION_TYPE_TRANSFER {
		destId, _ := strconv.ParseInt(spec.DestinationAccountId, 10, 64)
		dst, ok := lk.AccountBy[destId]

		if !ok || destId == 0 {
			return nil, Invalid("a transfer needs destination_account_id or destination_account_name", "the transfer has no destination account")
		}

		if err := refUsableAccount(dst, lk.Accounts, "destination account"); err != nil {
			return nil, err
		}

		if dst.AccountId == src.AccountId {
			return nil, Invalid("pick two different accounts", "a transfer from an account to itself")
		}

		if src.Currency == dst.Currency {
			if !destAmountGiven && spec.DestinationAmount != spec.Amount {
				spec.DestinationAmount = spec.Amount
			} else if destAmountGiven && spec.DestinationAmount != spec.Amount {
				warnings = append(warnings, fmt.Sprintf("both accounts are in %s but destination_amount (%d) differs from amount (%d)", src.Currency, spec.DestinationAmount, spec.Amount))
			}
		} else if !destAmountGiven && spec.DestinationAmount == 0 && spec.Amount != 0 {
			return nil, Invalid(fmt.Sprintf("the accounts are in %s and %s: pass destination_amount in %s hundredths (the app never converts a template's amount for you)", src.Currency, dst.Currency, dst.Currency), "a cross-currency transfer needs destination_amount")
		}
	}

	categoryId, _ := strconv.ParseInt(spec.CategoryId, 10, 64)
	cat, ok := lk.CategoryBy[categoryId]

	if !ok {
		return nil, NotFound("GET /machine/v1/categories lists them", "the template's category %s no longer exists", spec.CategoryId)
	}

	if err := refUsableCategory(cat, lk.Categories, typ); err != nil {
		return nil, err
	}

	if len(spec.TagIds) > refMaxTagsPerTransaction {
		return nil, Invalid("a template carries at most 10 tags", "%d tags given", len(spec.TagIds))
	}

	for _, s := range spec.TagIds {
		id, _ := strconv.ParseInt(s, 10, 64)
		t, ok := lk.TagBy[id]

		if !ok {
			return nil, NotFound("GET /machine/v1/tags lists them", "tag %s no longer exists", s)
		}

		if t.Hidden {
			return nil, Invalid("unhide the tag first (POST /machine/v1/tags/:id/hide {hidden: false}) or drop it", "the tag %q is hidden, and upstream refuses hidden tags on templates", t.Name)
		}
	}

	return warnings, nil
}

// refUsableAccount refuses a hidden or parent account the way upstream will, naming the fix
func refUsableAccount(a *models.Account, all []*models.Account, role string) error {
	if a.Type == models.ACCOUNT_TYPE_MULTI_SUB_ACCOUNTS {
		var subs []refCandidate

		for _, s := range all {
			if s.ParentAccountId == a.AccountId {
				subs = append(subs, refCandidate{Id: idString(s.AccountId), Name: s.Name})
			}
		}

		return Invalid("a parent account holds no balance of its own: use one of its sub-accounts", "the %s %q has sub-accounts", role, a.Name).WithDetails(map[string]any{"subAccounts": subs})
	}

	if a.Hidden {
		return Invalid("unhide it first (POST /machine/v1/accounts/:id/hide {hidden: false})", "the %s %q is hidden", role, a.Name)
	}

	return nil
}

// refUsableCategory refuses a primary, hidden or wrong-type category, naming the fix
func refUsableCategory(c *models.TransactionCategory, all []*models.TransactionCategory, typ models.TransactionType) error {
	want := map[models.TransactionType]models.TransactionCategoryType{
		models.TRANSACTION_TYPE_INCOME: models.CATEGORY_TYPE_INCOME, models.TRANSACTION_TYPE_EXPENSE: models.CATEGORY_TYPE_EXPENSE, models.TRANSACTION_TYPE_TRANSFER: models.CATEGORY_TYPE_TRANSFER,
	}[typ]

	if c.ParentCategoryId == models.LevelOneTransactionCategoryParentId {
		var subs []refCandidate

		for _, s := range all {
			if s.ParentCategoryId == c.CategoryId {
				subs = append(subs, refCandidate{Id: idString(s.CategoryId), Name: s.Name})
			}
		}

		return Invalid("transactions use SECONDARY categories: pick one of its children", "%q is a primary category", c.Name).WithDetails(map[string]any{"subCategories": subs})
	}

	if c.Type != want {
		return Invalid("a "+refTxnTypeNames[typ]+" needs a "+refCategoryTypeNames[want]+" category", "%q is a %s category", c.Name, refCategoryTypeNames[c.Type])
	}

	if c.Hidden {
		return Invalid("unhide it first (POST /machine/v1/categories/:id/hide {hidden: false})", "the category %q is hidden", c.Name)
	}

	return nil
}

// refTemplateUpstreamSchedule fills upstream's schedule pointers from a spec
func refTemplateUpstreamSchedule(spec *refTemplateSpec) (*models.TransactionScheduleFrequencyType, *string, *string, *string, *int16, error) {
	ft, err := refFreqType(spec.Frequency)

	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	fv := spec.FrequencyValue
	off := spec.UtcOffset
	var start, end *string

	if spec.Start != "" {
		s := spec.Start
		start = &s
	}

	if spec.End != "" {
		e := spec.End
		end = &e
	}

	return &ft, &fv, start, end, &off, nil
}

func refTemplateTagIds(spec *refTemplateSpec) []string {
	if spec.TagIds == nil {
		return []string{}
	}

	return spec.TagIds
}

func refTemplateCreateRequest(spec *refTemplateSpec, clientSessionId string) (*models.TransactionTemplateCreateRequest, error) {
	typ, err := refTemplateTxnType(spec.Type)

	if err != nil {
		return nil, err
	}

	categoryId, _ := strconv.ParseInt(spec.CategoryId, 10, 64)
	accountId, _ := strconv.ParseInt(spec.AccountId, 10, 64)
	destId, _ := strconv.ParseInt(spec.DestinationAccountId, 10, 64)

	req := &models.TransactionTemplateCreateRequest{
		TemplateType: models.TRANSACTION_TEMPLATE_TYPE_NORMAL, Name: spec.Name, Type: typ, CategoryId: categoryId, SourceAccountId: accountId,
		DestinationAccountId: destId, SourceAmount: spec.Amount, DestinationAmount: spec.DestinationAmount, HideAmount: spec.HideAmount,
		TagIds: refTemplateTagIds(spec), Comment: spec.Comment, ClientSessionId: clientSessionId,
	}

	if spec.Kind == "scheduled" {
		req.TemplateType = models.TRANSACTION_TEMPLATE_TYPE_SCHEDULE
		req.ScheduledFrequencyType, req.ScheduledFrequency, req.ScheduledStartDate, req.ScheduledEndDate, req.ScheduledTimezoneUtcOffset, err = refTemplateUpstreamSchedule(spec)

		if err != nil {
			return nil, err
		}
	}

	return req, nil
}

func refTemplateModifyRequest(spec *refTemplateSpec) (*models.TransactionTemplateModifyRequest, error) {
	typ, err := refTemplateTxnType(spec.Type)

	if err != nil {
		return nil, err
	}

	id, err := ResolveId("id", spec.Id)

	if err != nil {
		return nil, err
	}

	categoryId, _ := strconv.ParseInt(spec.CategoryId, 10, 64)
	accountId, _ := strconv.ParseInt(spec.AccountId, 10, 64)
	destId, _ := strconv.ParseInt(spec.DestinationAccountId, 10, 64)

	req := &models.TransactionTemplateModifyRequest{
		Id: id, Name: spec.Name, Type: typ, CategoryId: categoryId, SourceAccountId: accountId, DestinationAccountId: destId,
		SourceAmount: spec.Amount, DestinationAmount: spec.DestinationAmount, HideAmount: spec.HideAmount, TagIds: refTemplateTagIds(spec), Comment: spec.Comment,
	}

	if spec.Kind == "scheduled" {
		req.ScheduledFrequencyType, req.ScheduledFrequency, req.ScheduledStartDate, req.ScheduledEndDate, req.ScheduledTimezoneUtcOffset, err = refTemplateUpstreamSchedule(spec)

		if err != nil {
			return nil, err
		}
	}

	return req, nil
}

// refTemplateView is one template on the wire. Amounts are upstream's {type, amount} convention:
// positive magnitudes, each with its account's currency (R11).
type refTemplateView struct {
	Id                     string            `json:"id"`
	Kind                   string            `json:"kind"`
	Name                   string            `json:"name"`
	Type                   string            `json:"type"`
	TypeCode               int               `json:"typeCode"`
	CategoryId             string            `json:"categoryId"`
	CategoryName           string            `json:"categoryName"`
	SourceAccountId        string            `json:"sourceAccountId"`
	SourceAccountName      string            `json:"sourceAccountName"`
	SourceCurrency         string            `json:"sourceCurrency"`
	SourceAmount           int64             `json:"sourceAmount"`
	DestinationAccountId   string            `json:"destinationAccountId,omitempty"`
	DestinationAccountName string            `json:"destinationAccountName,omitempty"`
	DestinationCurrency    string            `json:"destinationCurrency,omitempty"`
	DestinationAmount      *int64            `json:"destinationAmount,omitempty"`
	HideAmount             bool              `json:"hideAmount"`
	TagIds                 []string          `json:"tagIds"`
	TagNames               []string          `json:"tagNames"`
	Comment                string            `json:"comment"`
	DisplayOrder           int32             `json:"displayOrder"`
	Hidden                 bool              `json:"hidden"`
	Schedule               *refScheduleView  `json:"schedule,omitempty"`
	Problems               []string          `json:"problems,omitempty"`
	Links                  map[string]string `json:"-"`
}

type refScheduleView struct {
	Frequency             string  `json:"frequency"`
	FrequencyCode         int     `json:"frequencyCode"`
	FrequencyValue        string  `json:"frequencyValue"`
	Describe              string  `json:"describe"`
	Start                 *string `json:"start"`
	End                   *string `json:"end"`
	UtcOffset             int16   `json:"utcOffset"`
	ScheduledAtUtcMinutes int16   `json:"scheduledAtUtcMinutes"`
	Paused                bool    `json:"paused"`
	Ended                 bool    `json:"ended"`
	NextOccurrence        *string `json:"nextOccurrence"`
}

func refTemplateViewOf(t *models.TransactionTemplate, lk *refLookups, now time.Time) *refTemplateView {
	v := &refTemplateView{
		Id: idString(t.TemplateId), Kind: refTemplateKind(t.TemplateType), Name: t.Name, Type: refTxnTypeNames[t.Type], TypeCode: int(t.Type),
		CategoryId: idString(t.CategoryId), SourceAccountId: idString(t.AccountId), SourceAmount: t.Amount, HideAmount: t.HideAmount,
		TagIds: []string{}, TagNames: []string{}, Comment: t.Comment, DisplayOrder: t.DisplayOrder, Hidden: t.Hidden,
	}

	if c, ok := lk.CategoryBy[t.CategoryId]; ok {
		v.CategoryName = c.Name
	} else {
		v.Problems = append(v.Problems, "its category no longer exists")
	}

	if a, ok := lk.AccountBy[t.AccountId]; ok {
		v.SourceAccountName, v.SourceCurrency = a.Name, a.Currency
	} else {
		v.Problems = append(v.Problems, "its account no longer exists")
	}

	if t.Type == models.TRANSACTION_TYPE_TRANSFER {
		v.DestinationAccountId = idString(t.RelatedAccountId)
		amt := t.RelatedAccountAmount
		v.DestinationAmount = &amt

		if a, ok := lk.AccountBy[t.RelatedAccountId]; ok {
			v.DestinationAccountName, v.DestinationCurrency = a.Name, a.Currency
		} else {
			v.Problems = append(v.Problems, "its destination account no longer exists")
		}
	}

	for _, id := range t.GetTagIds() {
		v.TagIds = append(v.TagIds, idString(id))

		if tag, ok := lk.TagBy[id]; ok {
			v.TagNames = append(v.TagNames, tag.Name)
		} else {
			v.TagNames = append(v.TagNames, "")
		}
	}

	if t.TemplateType == models.TRANSACTION_TEMPLATE_TYPE_SCHEDULE {
		spec := refTemplateSpecOf(t)
		s := &refScheduleView{
			Frequency: spec.Frequency, FrequencyCode: int(t.ScheduledFrequencyType), FrequencyValue: t.ScheduledFrequency,
			Describe: refDescribeFrequency(t.ScheduledFrequencyType, t.ScheduledFrequency), UtcOffset: t.ScheduledTimezoneUtcOffset,
			ScheduledAtUtcMinutes: t.ScheduledAt, Paused: t.ScheduledFrequencyType == models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DISABLED,
			Ended: t.ScheduledEndTime != nil && *t.ScheduledEndTime < now.Unix(),
		}

		if spec.Start != "" {
			s.Start = &spec.Start
		}

		if spec.End != "" {
			s.End = &spec.End
		}

		if next := refScheduleOccurrences(t, now.Unix(), now.Unix()+400*86400, 1); len(next) == 1 {
			tz := time.FixedZone("Template Timezone", int(t.ScheduledTimezoneUtcOffset)*60)
			d := time.Unix(next[0], 0).In(tz).Format("2006-01-02")
			s.NextOccurrence = &d
		}

		v.Schedule = s
	}

	return v
}

func refLoadTemplates(mc *Ctx, includeScheduled bool) ([]*models.TransactionTemplate, error) {
	all, err := services.TransactionTemplates.GetAllTemplatesByUid(mc.Web, mc.Uid, models.TRANSACTION_TEMPLATE_TYPE_NORMAL)

	if err != nil {
		return nil, err
	}

	if includeScheduled {
		sched, err := services.TransactionTemplates.GetAllTemplatesByUid(mc.Web, mc.Uid, models.TRANSACTION_TEMPLATE_TYPE_SCHEDULE)

		if err != nil {
			return nil, err
		}

		all = append(all, sched...)
	}

	return all, nil
}

func refTemplateName(t *models.TransactionTemplate) string { return t.Name }
func refTemplateId(t *models.TransactionTemplate) int64    { return t.TemplateId }

func refResolveTemplate(mc *Ctx, ref string) (*models.TransactionTemplate, error) {
	all, err := refLoadTemplates(mc, mc.Config.EnableScheduledTransaction)

	if err != nil {
		return nil, err
	}

	t, err := refResolveOne("template", ref, all, refTemplateId, refTemplateName, func(t *models.TransactionTemplate) string { return refTemplateKind(t.TemplateType) }, "GET /machine/v1/templates?include_hidden=true lists them")

	if err != nil && !mc.Config.EnableScheduledTransaction {
		if f := toFail(err); f.Code == CodeNotFound {
			f.Hint += " (scheduled transactions are switched off: [user] enable_scheduled_transaction in conf/ezbookkeeping.ini)"
		}
	}

	return t, err
}

func refHandleTemplateList(mc *Ctx) (any, error) {
	includeHidden, err := mc.QueryBool("include_hidden", false)

	if err != nil {
		return nil, err
	}

	kind := strings.ToLower(mc.Query("kind"))
	schedOn := mc.Config.EnableScheduledTransaction

	switch kind {
	case "", "all":
		kind = "all"
	case "normal", "template":
		kind = "normal"
	case "scheduled", "schedule":
		kind = "scheduled"

		if !schedOn {
			return nil, NewFail(CodeForbidden, "switch on [user] enable_scheduled_transaction in conf/ezbookkeeping.ini and restart", "scheduled transactions are switched off upstream")
		}
	default:
		return nil, Invalid("kind is normal, scheduled or all", "unknown kind %q", kind)
	}

	all, err := refLoadTemplates(mc, schedOn)

	if err != nil {
		return nil, err
	}

	lk, err := refLoadLookups(mc)

	if err != nil {
		return nil, err
	}

	var pool []*models.TransactionTemplate

	for _, t := range all {
		k := refTemplateKind(t.TemplateType)

		if (kind == "all" || kind == k) && (includeHidden || !t.Hidden) {
			pool = append(pool, t)
		}
	}

	filters := map[string]any{"kind": kind, "includeHidden": includeHidden}

	if name := mc.Query("name"); name != "" {
		pool = refMatchByName(pool, name, refTemplateName)
		filters["name"] = name
	}

	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].TemplateType != pool[j].TemplateType {
			return pool[i].TemplateType < pool[j].TemplateType
		}

		if pool[i].DisplayOrder != pool[j].DisplayOrder {
			return pool[i].DisplayOrder < pool[j].DisplayOrder
		}

		return pool[i].TemplateId < pool[j].TemplateId
	})

	now := time.Now()
	rows := make([]*refTemplateView, 0, len(pool))

	for _, t := range pool {
		rows = append(rows, refTemplateViewOf(t, lk, now))
	}

	if kind == "all" && !schedOn {
		mc.SetMeta("note", "scheduled transactions are switched off upstream ([user] enable_scheduled_transaction); only normal templates are listed")
	}

	return map[string]any{"templates": rows, "count": len(rows), "filters": filters, "scheduledTransactionsEnabled": schedOn, "cronCreatesTransactions": mc.Config.EnableCreateScheduledTransaction}, nil
}

func refHandleTemplateGet(mc *Ctx) (any, error) {
	t, err := refResolveTemplate(mc, mc.Param("id"))

	if err != nil {
		return nil, err
	}

	lk, err := refLoadLookups(mc)

	if err != nil {
		return nil, err
	}

	return map[string]any{"template": refTemplateViewOf(t, lk, time.Now())}, nil
}

// refTemplatePreview renders a spec for a dry run, with names beside ids
func refTemplatePreview(spec *refTemplateSpec, lk *refLookups) map[string]any {
	out := map[string]any{
		"kind": spec.Kind, "name": spec.Name, "type": spec.Type, "amount": spec.Amount, "hideAmount": spec.HideAmount, "comment": spec.Comment, "tagIds": spec.TagIds,
	}
	accountId, _ := strconv.ParseInt(spec.AccountId, 10, 64)

	if a, ok := lk.AccountBy[accountId]; ok {
		out["account"] = map[string]any{"id": spec.AccountId, "name": a.Name, "currency": a.Currency}
		out["currency"] = a.Currency
	}

	if spec.Type == "transfer" {
		destId, _ := strconv.ParseInt(spec.DestinationAccountId, 10, 64)

		if a, ok := lk.AccountBy[destId]; ok {
			out["destinationAccount"] = map[string]any{"id": spec.DestinationAccountId, "name": a.Name, "currency": a.Currency}
			out["destinationCurrency"] = a.Currency
		}

		out["destinationAmount"] = spec.DestinationAmount
	}

	categoryId, _ := strconv.ParseInt(spec.CategoryId, 10, 64)

	if c, ok := lk.CategoryBy[categoryId]; ok {
		out["category"] = map[string]any{"id": spec.CategoryId, "name": c.Name}
	}

	var tagNames []string

	for _, s := range spec.TagIds {
		id, _ := strconv.ParseInt(s, 10, 64)

		if t, ok := lk.TagBy[id]; ok {
			tagNames = append(tagNames, t.Name)
		}
	}

	out["tagNames"] = tagNames

	if spec.Kind == "scheduled" {
		ft, _ := refFreqType(spec.Frequency)
		out["schedule"] = map[string]any{
			"frequency": spec.Frequency, "frequencyValue": spec.FrequencyValue, "describe": refDescribeFrequency(ft, spec.FrequencyValue),
			"start": spec.Start, "end": spec.End, "utcOffset": spec.UtcOffset, "createdAt": "00:00 in the schedule's timezone",
		}
	}

	return out
}

// refTemplateDiff lists the fields that differ between two specs
func refTemplateDiff(before, after *refTemplateSpec, lk *refLookups) []refFieldChange {
	decode := func(v any) map[string]any {
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(refMustJSON(v)))
		dec.UseNumber()
		_ = dec.Decode(&m)

		return m
	}

	bm, am := decode(before), decode(after)

	keys := make([]string, 0, len(am))
	seen := map[string]bool{}

	for k := range bm {
		keys, seen[k] = append(keys, k), true
	}

	for k := range am {
		if !seen[k] {
			keys = append(keys, k)
		}
	}

	sort.Strings(keys)

	named := func(field string, v any) any {
		s, _ := v.(string)
		id, _ := strconv.ParseInt(s, 10, 64)

		switch field {
		case "accountId", "destinationAccountId":
			if a, ok := lk.AccountBy[id]; ok {
				return map[string]any{"id": s, "name": a.Name, "currency": a.Currency}
			}
		case "categoryId":
			if c, ok := lk.CategoryBy[id]; ok {
				return map[string]any{"id": s, "name": c.Name}
			}
		}

		return v
	}

	changes := []refFieldChange{}

	for _, k := range keys {
		if k == "id" {
			continue
		}

		if string(refMustJSON(bm[k])) != string(refMustJSON(am[k])) {
			changes = append(changes, refFieldChange{Id: before.Id, Name: before.Name, Field: k, From: named(k, bm[k]), To: named(k, am[k])})
		}
	}

	return changes
}

func refHandleTemplateCreate(mc *Ctx) (any, error) {
	var body refTemplateBody

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	if err := refCheckIdemKey(body.IdempotencyKey); err != nil {
		return nil, err
	}

	now := time.Now()

	return RunWrite(mc, body.WriteOpts, func() (*Plan, error) {
		lk, err := refLoadLookups(mc)

		if err != nil {
			return nil, err
		}

		spec := &refTemplateSpec{TagIds: []string{}, DestinationAccountId: "0"}
		warnings, err := refApplyTemplateBody(spec, &body, lk, mc.Loc, now, true)

		if err != nil {
			return nil, err
		}

		if spec.Kind == "scheduled" && !mc.Config.EnableScheduledTransaction {
			return nil, NewFail(CodeForbidden, "switch on [user] enable_scheduled_transaction in conf/ezbookkeeping.ini and restart", "scheduled transactions are switched off upstream")
		}

		if spec.Kind == "scheduled" && !mc.Config.EnableCreateScheduledTransaction {
			warnings = append(warnings, "the cron that turns schedules into transactions is off ([cron] enable_create_scheduled_transaction); this schedule will be saved but create nothing until it is on")
		}

		preview := refTemplatePreview(spec, lk)
		preview["action"] = "create"

		return &Plan{Changes: map[string]int{"create": 1}, Count: 1, Preview: preview, Fingerprinted: spec, Warnings: warnings, State: spec}, nil
	}, func(p *Plan) (any, error) {
		if v, ok := refIdemGet(mc, body.IdempotencyKey); ok {
			return v, nil
		}

		spec := p.State.(*refTemplateSpec)
		req, err := refTemplateCreateRequest(spec, body.IdempotencyKey)

		if err != nil {
			return nil, err
		}

		res, err := mc.CallUpstream(api.TransactionTemplates.TemplateCreateHandler, "POST", nil, req)

		if err != nil {
			return nil, err
		}

		id, err := refCreatedId(res)

		if err != nil {
			return nil, err
		}

		if err := refJournalCreate(mc, "template", []int64{id}, nil, "create "+spec.Kind+" template "+idString(id)); err != nil {
			return nil, err
		}

		out, err := refTemplateResult(mc, id)

		if err != nil {
			return nil, err
		}

		refIdemPut(mc, body.IdempotencyKey, out)

		return out, nil
	})
}

func refTemplateResult(mc *Ctx, id int64) (map[string]any, error) {
	t, err := services.TransactionTemplates.GetTemplateByTemplateId(mc.Web, mc.Uid, id)

	if err != nil {
		return nil, err
	}

	lk, err := refLoadLookups(mc)

	if err != nil {
		return nil, err
	}

	return map[string]any{"template": refTemplateViewOf(t, lk, time.Now())}, nil
}

func refHandleTemplatePatch(mc *Ctx) (any, error) {
	var body refTemplateBody

	if err := mc.BindBody(&body); err != nil {
		return nil, err
	}

	ref := mc.Param("id")
	now := time.Now()

	type state struct {
		Before, After refTemplateSpec
	}

	return RunWrite(mc, body.WriteOpts, func() (*Plan, error) {
		t, err := refResolveTemplate(mc, ref)

		if err != nil {
			return nil, err
		}

		lk, err := refLoadLookups(mc)

		if err != nil {
			return nil, err
		}

		before := refTemplateSpecOf(t)
		after := refTemplateSpecOf(t)
		warnings, err := refApplyTemplateBody(&after, &body, lk, mc.Loc, now, false)

		if err != nil {
			return nil, err
		}

		changes := refTemplateDiff(&before, &after, lk)
		plan := &Plan{Preview: changes, Fingerprinted: []any{before, after}, Warnings: warnings, State: &state{Before: before, After: after}, Changes: map[string]int{"update": 0}}

		if len(changes) > 0 {
			plan.Changes["update"] = 1
			plan.Count = 1
		} else {
			plan.Changes["unchanged"] = 1
		}

		return plan, nil
	}, func(p *Plan) (any, error) {
		st := p.State.(*state)
		id, _ := strconv.ParseInt(st.Before.Id, 10, 64)

		if p.Count > 0 {
			if err := refApplyTemplateSpec(mc, &st.After); err != nil {
				return nil, err
			}

			if err := refJournalRestore(mc, "ref.restore_template", st.Before, st.After, "edit template "+st.Before.Id); err != nil {
				return nil, err
			}
		}

		out, err := refTemplateResult(mc, id)

		if err != nil {
			return nil, err
		}

		out["updated"] = p.Count

		return out, nil
	})
}

func refApplyTemplateSpec(mc *Ctx, spec *refTemplateSpec) error {
	req, err := refTemplateModifyRequest(spec)

	if err != nil {
		return err
	}

	_, err = mc.CallUpstream(api.TransactionTemplates.TemplateModifyHandler, "POST", nil, req)

	return err
}

func refInvRestoreTemplate(mc *Ctx, payload, check json.RawMessage) error {
	var want refTemplateSpec

	if err := json.Unmarshal(payload, &want); err != nil {
		return err
	}

	id, err := ResolveId("id", want.Id)

	if err != nil {
		return err
	}

	t, err := services.TransactionTemplates.GetTemplateByTemplateId(mc.Web, mc.Uid, id)

	if err != nil || t == nil {
		return Conflict("the template was deleted since; nothing to restore", "template %s no longer exists", want.Id)
	}

	current := refTemplateSpecOf(t)

	if !refCheckMatches(current, check) {
		return Conflict("the template was edited again since; nothing was changed", "template %s changed since the write", want.Id)
	}

	if refCheckMatches(current, refMustJSON(want)) {
		return nil
	}

	return refApplyTemplateSpec(mc, &want)
}

func refTemplateSiblings(mc *Ctx, ref string) (int64, []refSibling, error) {
	t, err := refResolveTemplate(mc, ref)

	if err != nil {
		return 0, nil, err
	}

	list, err := services.TransactionTemplates.GetAllTemplatesByUid(mc.Web, mc.Uid, t.TemplateType)

	if err != nil {
		return 0, nil, err
	}

	sibs := make([]refSibling, 0, len(list))

	for _, o := range list {
		sibs = append(sibs, refSibling{Id: o.TemplateId, Name: o.Name, Order: o.DisplayOrder})
	}

	return t.TemplateId, sibs, nil
}

func refTemplateHideTarget(mc *Ctx, ref string, hidden bool) (*refHideTarget, error) {
	t, err := refResolveTemplate(mc, ref)

	if err != nil {
		return nil, err
	}

	h := &refHideTarget{Id: t.TemplateId, Name: t.Name, Hidden: t.Hidden}

	if t.TemplateType == models.TRANSACTION_TEMPLATE_TYPE_SCHEDULE {
		note := "hiding a schedule does NOT pause it: upstream's cron ignores the hidden flag. To pause, PATCH /machine/v1/templates/:id {frequency: \"disabled\"}"
		h.Warnings = append(h.Warnings, note)
		h.Extra = map[string]any{"pausesSchedule": false, "note": note}
	}

	return h, nil
}

func refTemplateDeleteTarget(mc *Ctx, ref string) (*refDeleteTarget, error) {
	t, err := refResolveTemplate(mc, ref)

	if err != nil {
		return nil, err
	}

	preview := map[string]any{"action": "delete", "id": idString(t.TemplateId), "name": t.Name, "kind": refTemplateKind(t.TemplateType)}
	var warnings []string

	if t.TemplateType == models.TRANSACTION_TEMPLATE_TYPE_SCHEDULE {
		warnings = append(warnings, "transactions this schedule already created are kept; only future ones stop")
	}

	return &refDeleteTarget{Id: t.TemplateId, Preview: preview, Warnings: warnings}, nil
}

func refTemplateRows(mc *Ctx, ids []int64) (map[int64]refRow, error) {
	var rows []*models.TransactionTemplate

	if err := refDB(mc).NewSession(mc.Web).Where("uid=?", mc.Uid).In("template_id", ids).Find(&rows); err != nil {
		return nil, err
	}

	out := map[int64]refRow{}

	for _, r := range rows {
		out[r.TemplateId] = refRow{Id: r.TemplateId, Name: r.Name, Hidden: r.Hidden, Order: r.DisplayOrder, Deleted: r.Deleted}
	}

	return out, nil
}

var refTemplateKindDef = &refKind{
	Name: "template",
	List: "GET /machine/v1/templates",
	Rows: refTemplateRows,
	Delete: func(mc *Ctx, id int64) error {
		_, err := mc.CallUpstream(api.TransactionTemplates.TemplateDeleteHandler, "POST", nil, map[string]any{"id": idString(id)})
		return err
	},
	Undelete: func(mc *Ctx, ids []int64) (int, error) {
		return refUndeleteGeneric(mc, &models.TransactionTemplate{}, "template_id", ids)
	},
}

// refUpcomingRow is one future transaction a schedule will create
type refUpcomingRow struct {
	TemplateId             string   `json:"templateId"`
	TemplateName           string   `json:"templateName"`
	Date                   string   `json:"date"`
	LocalDate              string   `json:"localDate"`
	Time                   int64    `json:"time"`
	At                     string   `json:"at"`
	Type                   string   `json:"type"`
	CategoryId             string   `json:"categoryId"`
	CategoryName           string   `json:"categoryName"`
	SourceAccountId        string   `json:"sourceAccountId"`
	SourceAccountName      string   `json:"sourceAccountName"`
	SourceCurrency         string   `json:"sourceCurrency"`
	SourceAmount           int64    `json:"sourceAmount"`
	DestinationAccountId   string   `json:"destinationAccountId,omitempty"`
	DestinationAccountName string   `json:"destinationAccountName,omitempty"`
	DestinationCurrency    string   `json:"destinationCurrency,omitempty"`
	DestinationAmount      *int64   `json:"destinationAmount,omitempty"`
	TagNames               []string `json:"tagNames"`
	Comment                string   `json:"comment"`
	Hidden                 bool     `json:"hidden"`
}

// refHandleUpcoming is GET /schedules/upcoming — composed: the cron's own per-day predicate
// (refScheduleFiresAt) walked over the window, firing nothing
func refHandleUpcoming(mc *Ctx) (any, error) {
	days, err := mc.QueryInt("days", 30)

	if err != nil {
		return nil, err
	}

	if days < 1 || days > 3660 {
		return nil, Invalid("days is 1-3660", "days %d is out of range", days)
	}

	limit, err := mc.QueryInt("limit", int64(DefaultLimit))

	if err != nil {
		return nil, err
	}

	if limit < 1 {
		limit = int64(DefaultLimit)
	}

	if limit > int64(MaxLimit) {
		limit = int64(MaxLimit)
	}

	now := time.Now().In(mc.Loc)
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, mc.Loc)

	if v := mc.Query("from"); v != "" {
		if from, err = ParseDate("from", v, mc.Loc); err != nil {
			return nil, err
		}
	}

	to := from.AddDate(0, 0, int(days))
	fromUnix, toUnix := from.Unix(), to.Unix()-1

	templates, err := services.TransactionTemplates.GetAllTemplatesByUid(mc.Web, mc.Uid, models.TRANSACTION_TEMPLATE_TYPE_SCHEDULE)

	if err != nil {
		return nil, err
	}

	if refs := mc.QueryList("template_ids"); len(refs) > 0 {
		var picked []*models.TransactionTemplate
		seen := map[int64]bool{}

		for _, r := range refs {
			t, err := refResolveOne("scheduled template", r, templates, refTemplateId, refTemplateName, nil, "GET /machine/v1/templates?kind=scheduled lists them")

			if err != nil {
				return nil, err
			}

			if !seen[t.TemplateId] {
				seen[t.TemplateId] = true
				picked = append(picked, t)
			}
		}

		templates = picked
	}

	lk, err := refLoadLookups(mc)

	if err != nil {
		return nil, err
	}

	var rows []refUpcomingRow
	var perTemplate []map[string]any
	truncated := false

	for _, t := range templates {
		occ := refScheduleOccurrences(t, fromUnix, toUnix, int(limit)+1)
		v := refTemplateViewOf(t, lk, now)
		tz := time.FixedZone("Template Timezone", int(t.ScheduledTimezoneUtcOffset)*60)

		perTemplate = append(perTemplate, map[string]any{
			"templateId": v.Id, "name": v.Name, "describe": refDescribeFrequency(t.ScheduledFrequencyType, t.ScheduledFrequency),
			"occurrences": len(occ), "paused": t.ScheduledFrequencyType == models.TRANSACTION_SCHEDULE_FREQUENCY_TYPE_DISABLED, "hidden": t.Hidden, "problems": v.Problems,
		})

		for _, tx := range occ {
			row := refUpcomingRow{
				TemplateId: v.Id, TemplateName: v.Name, Date: time.Unix(tx, 0).In(tz).Format("2006-01-02"), LocalDate: time.Unix(tx, 0).In(mc.Loc).Format("2006-01-02"),
				Time: tx, At: time.Unix(tx, 0).UTC().Format(time.RFC3339), Type: v.Type, CategoryId: v.CategoryId, CategoryName: v.CategoryName,
				SourceAccountId: v.SourceAccountId, SourceAccountName: v.SourceAccountName, SourceCurrency: v.SourceCurrency, SourceAmount: v.SourceAmount,
				DestinationAccountId: v.DestinationAccountId, DestinationAccountName: v.DestinationAccountName, DestinationCurrency: v.DestinationCurrency,
				DestinationAmount: v.DestinationAmount, TagNames: v.TagNames, Comment: v.Comment, Hidden: v.Hidden,
			}
			rows = append(rows, row)
		}
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Time != rows[j].Time {
			return rows[i].Time < rows[j].Time
		}

		return rows[i].TemplateId < rows[j].TemplateId
	})

	if len(rows) > int(limit) {
		rows = rows[:limit]
		truncated = true
		mc.Truncated(int(limit))
	}

	if rows == nil {
		rows = []refUpcomingRow{}
	}

	if perTemplate == nil {
		perTemplate = []map[string]any{}
	}

	window := map[string]any{
		"start": from.Format("2006-01-02"), "end": to.AddDate(0, 0, -1).Format("2006-01-02"), "days": days,
		"startUnix": fromUnix, "endUnix": toUnix, "timezone": mc.Loc.String(),
	}

	var notes []string
	notes = append(notes, "computed with the cron's own calendar rule; nothing was created")

	if !mc.Config.EnableCreateScheduledTransaction {
		notes = append(notes, "the cron is OFF ([cron] enable_create_scheduled_transaction): none of these will actually be created until it is switched on")
	}

	notes = append(notes, "hidden schedules still fire; paused (frequency disabled) ones do not")

	return map[string]any{
		"occurrences": rows, "count": len(rows), "truncated": truncated, "window": window, "templates": perTemplate,
		"cronEnabled": mc.Config.EnableCreateScheduledTransaction, "notes": notes,
	}, nil
}

// ---------------------------------------------------------------------------------------------
// the route table and registration
// ---------------------------------------------------------------------------------------------

var refFeatureScheduled = func(c *settings.Config) (bool, string) {
	return c.EnableScheduledTransaction, "[user] enable_scheduled_transaction"
}

func init() {
	refCategoryKind.Hide = api.TransactionCategories.CategoryHideHandler
	refCategoryKind.Move = api.TransactionCategories.CategoryMoveHandler
	refTagKind.Hide = api.TransactionTags.TagHideHandler
	refTagKind.Move = api.TransactionTags.TagMoveHandler
	refTagGroupKind.Move = api.TransactionTagGroups.TagGroupMoveHandler
	refTemplateKindDef.Hide = api.TransactionTemplates.TemplateHideHandler
	refTemplateKindDef.Move = api.TransactionTemplates.TemplateMoveHandler
	refInsightKind.Hide = api.InsightsExplorers.InsightsExplorerHideHandler
	refInsightKind.Move = api.InsightsExplorers.InsightsExplorerMoveHandler

	for _, k := range []*refKind{refCategoryKind, refTagKind, refTagGroupKind, refTemplateKindDef, refInsightKind} {
		refRegisterKind(k)
	}

	RegisterInverse("ref.restore_category_fields", refInvRestoreCategory)
	RegisterInverse("ref.restore_tag_fields", refInvRestoreTag)
	RegisterInverse("ref.restore_tag_group_fields", refInvRestoreTagGroup)
	RegisterInverse("ref.restore_template", refInvRestoreTemplate)
	RegisterInverse("ref.restore_insight", refInvRestoreInsight)

	registerRoutes(refReferenceRoutes)
}

func refReferenceRoutes() []RouteDef {
	w := func(method, path, summary string, h HandlerFunc) RouteDef {
		return RouteDef{Method: method, Path: path, Tier: TierWrite, DryRunnable: true, Summary: summary, Handler: h}
	}
	a := func(method, path, summary string, h HandlerFunc) RouteDef {
		return RouteDef{Method: method, Path: path, Tier: TierAdmin, DryRunnable: true, Summary: summary, Handler: h}
	}
	r := func(path, summary string, h HandlerFunc, untrusted ...string) RouteDef {
		return RouteDef{Method: "GET", Path: path, Tier: TierRead, Summary: summary, Handler: h, Untrusted: untrusted}
	}
	hide := func(k *refKind, t func(*Ctx, string, bool) (*refHideTarget, error)) HandlerFunc {
		return func(mc *Ctx) (any, error) { return refHandleHide(mc, k, t) }
	}
	move := func(k *refKind, s func(*Ctx, string) (int64, []refSibling, error)) HandlerFunc {
		return func(mc *Ctx) (any, error) { return refHandleMove(mc, k, s) }
	}
	del := func(k *refKind, t func(*Ctx, string) (*refDeleteTarget, error)) HandlerFunc {
		return func(mc *Ctx) (any, error) { return refHandleDelete(mc, k, mc.Param("id"), t) }
	}

	return []RouteDef{
		// categories
		r("/categories", "Categories, primaries with secondaries nested. Args: type (income|expense|transfer), include_hidden (false), parent_id (id or name → its children, flat), name (exact, then case-insensitive → flat rows), flat.", refHandleCategoryList, "name", "comment"),
		r("/categories/:id", "One category (id or name), with its secondaries when primary.", refHandleCategoryGet, "name", "comment"),
		w("POST", "/categories", "Create a category. Body: name, type (omit under a parent), parent_id|parent_name (omit for a primary), color, icon, comment, idempotency_key; dry_run default true.", refHandleCategoryCreate),
		w("POST", "/categories/batch", "Create primaries with their secondaries (the web UI's default-categories preset). Body: categories: [{name, type, color, icon, comment, sub_categories: [{name, color, icon, comment}]}].", refHandleCategoryBatch),
		w("PATCH", "/categories/:id", "Edit a category. Body: name, parent_id|parent_name (secondaries only, same type), color, icon, comment.", refHandleCategoryPatch),
		w("POST", "/categories/:id/hide", "Hide or show a category. Body: hidden (default true).", hide(refCategoryKind, refCategoryHideTarget)),
		w("POST", "/categories/:id/move", "Reorder a category among its siblings (same type and parent). Body: to_index (0-based).", move(refCategoryKind, refCategorySiblings)),
		a("DELETE", "/categories/:id", "Delete a category and its secondaries; refused while transactions or templates use it, naming the count.", del(refCategoryKind, refCategoryDeleteTarget)),

		// tags and tag groups
		r("/tags", "Tags. Args: include_hidden (false), group_id|group_name (0 = ungrouped), name.", refHandleTagList, "name"),
		r("/tags/:id", "One tag (id or name), with how many transactions carry it.", refHandleTagGet, "name"),
		w("POST", "/tags", "Create a tag. Body: name, group_id|group_name, idempotency_key.", refHandleTagCreate),
		w("POST", "/tags/batch", "Create many tags in one group. Body: tags: [\"name\", …] or [{name}], group_id|group_name, skip_existing (true).", refHandleTagBatch),
		w("PATCH", "/tags/:id", "Rename a tag or move it to another group. Body: name, group_id|group_name (\"0\" = no group).", refHandleTagPatch),
		w("POST", "/tags/:id/hide", "Hide or show a tag. Body: hidden (default true).", hide(refTagKind, refTagHideTarget)),
		w("POST", "/tags/:id/move", "Reorder a tag within its group. Body: to_index (0-based).", move(refTagKind, refTagSiblings)),
		a("DELETE", "/tags/:id", "Delete a tag; refused while transactions or templates carry it, naming the count.", del(refTagKind, refTagDeleteTarget)),
		r("/tag-groups", "Tag groups with their tag counts. Args: name.", refHandleTagGroupList, "name"),
		r("/tag-groups/:id", "One tag group (id or name) with its tags.", refHandleTagGroupGet, "name"),
		w("POST", "/tag-groups", "Create a tag group. Body: name, idempotency_key.", refHandleTagGroupCreate),
		w("PATCH", "/tag-groups/:id", "Rename a tag group. Body: name.", refHandleTagGroupPatch),
		w("POST", "/tag-groups/:id/move", "Reorder a tag group. Body: to_index (0-based).", move(refTagGroupKind, refTagGroupSiblings)),
		a("DELETE", "/tag-groups/:id", "Delete an empty tag group; refused while it holds tags.", del(refTagGroupKind, refTagGroupDeleteTarget)),

		// templates and schedules
		r("/templates", "Transaction templates and scheduled transactions. Args: kind (normal|scheduled|all, default all), include_hidden (false), name.", refHandleTemplateList, "name", "comment"),
		r("/templates/:id", "One template or schedule (id or name), with its next occurrence.", refHandleTemplateGet, "name", "comment"),
		w("POST", "/templates", "Create a template (kind normal) or a scheduled transaction (kind scheduled). Body: kind, name, type, account_id|account_name, destination_account_id|destination_account_name, amount, destination_amount, category_id|category_name, tag_ids|tag_names, comment, hide_amount; scheduled: frequency (daily|weekly|monthly|yearly|every_n_days|disabled), frequency_value, start, end, timezone; idempotency_key.", refHandleTemplateCreate),
		w("PATCH", "/templates/:id", "Edit a template or schedule: any create field; absent means unchanged; end \"\" removes the end date; frequency \"disabled\" pauses a schedule.", refHandleTemplatePatch),
		w("POST", "/templates/:id/hide", "Hide or show a template. For a schedule this does NOT pause it (upstream's cron ignores hidden); the response says so. Body: hidden (default true).", hide(refTemplateKindDef, refTemplateHideTarget)),
		w("POST", "/templates/:id/move", "Reorder a template among its kind. Body: to_index (0-based).", move(refTemplateKindDef, refTemplateSiblings)),
		a("DELETE", "/templates/:id", "Delete a template or schedule (transactions it already created are kept).", del(refTemplateKindDef, refTemplateDeleteTarget)),
		{Method: "GET", Path: "/schedules/upcoming", Tier: TierRead, Composed: true, Feature: refFeatureScheduled, Handler: refHandleUpcoming, Untrusted: []string{"templateName", "comment"},
			Summary: "What the scheduled transactions will create in the window, computed with the cron's own calendar rule, firing nothing. Args: days (30), from (YYYY-MM-DD, default today), template_ids (ids or names), limit."},

		// insights
		r("/insights", "Saved Insights Explorer definitions (not numbers — those come from /analytics). Args: include_hidden (false), name.", refHandleInsightList, "name"),
		r("/insights/:id", "One saved insight (id or name) with its definition as data.", refHandleInsightGet, "name", "data"),
		w("POST", "/insights", "Save an insight definition. Body: name, definition (object), idempotency_key.", refHandleInsightCreate),
		w("PATCH", "/insights/:id", "Rename an insight or replace its definition. Body: name, definition.", refHandleInsightPatch),
		w("POST", "/insights/:id/hide", "Hide or show an insight. Body: hidden (default true).", hide(refInsightKind, refInsightHideTarget)),
		w("POST", "/insights/:id/move", "Reorder an insight. Body: to_index (0-based).", move(refInsightKind, refInsightSiblings)),
		a("DELETE", "/insights/:id", "Delete a saved insight.", del(refInsightKind, refInsightDeleteTarget)),
	}
}
