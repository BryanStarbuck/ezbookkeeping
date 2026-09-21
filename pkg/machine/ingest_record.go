package machine

import (
	"time"

	"xorm.io/xorm"

	"github.com/mayswind/ezbookkeeping/pkg/datastore"
	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/log"
	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// ingest_record.go — machine_import_record, the durable duplicate authority (apis.mdx §14.6).
//
// ezBookkeeping's Transaction has no imported_id column and we never smuggle one into `comment`,
// so this table remembers which statement row (import_id) became which transaction. The plan asks
// this table, and only this table, "does this row already exist?".

// MachineImportRecord is one statement row that became (or was linked to) a transaction.
// Columns exactly per apis.mdx §14.6; (uid, import_id) is the primary key, so import_id is unique
// per uid.
type MachineImportRecord struct {
	Uid             int64  `xorm:"PK INDEX(IDX_machine_import_record_uid_run_id) INDEX(IDX_machine_import_record_uid_transaction_id) NOT NULL"`
	ImportId        string `xorm:"PK VARCHAR(255) NOT NULL"`
	TransactionId   int64  `xorm:"INDEX(IDX_machine_import_record_uid_transaction_id) NOT NULL"`
	AccountKey      string `xorm:"VARCHAR(255) NOT NULL"`
	SourceFile      string `xorm:"VARCHAR(1024)"`
	RunId           string `xorm:"INDEX(IDX_machine_import_record_uid_run_id) VARCHAR(64) NOT NULL"`
	CreatedUnixTime int64  `xorm:"NOT NULL"`
}

func init() {
	registerTable(new(MachineImportRecord))
}

const ingInChunk = 400

func ingDB(mc *Ctx) (*datastore.Database, error) {
	if datastore.Container == nil || datastore.Container.UserDataStore == nil {
		return nil, NewFail(CodeNotReady, "wait for the server to finish starting", "the database is not open")
	}

	return datastore.Container.UserDataStore.Choose(mc.Uid), nil
}

// ingLoadRecords returns the records for the given import ids, keyed by import id
func ingLoadRecords(mc *Ctx, importIds []string) (map[string]*MachineImportRecord, error) {
	out := make(map[string]*MachineImportRecord, len(importIds))

	if len(importIds) == 0 {
		return out, nil
	}

	db, err := ingDB(mc)

	if err != nil {
		return nil, err
	}

	for start := 0; start < len(importIds); start += ingInChunk {
		end := start + ingInChunk

		if end > len(importIds) {
			end = len(importIds)
		}

		var recs []*MachineImportRecord

		if err := db.NewSession(mc.Web).Where("uid=?", mc.Uid).In("import_id", importIds[start:end]).Find(&recs); err != nil {
			errfile.Caught("reading the import records for a batch of import ids", err)
			return nil, NewFail(CodeUpstreamError, "check ~/T/ezbookkeeping/error.err; the machine_import_record table could not be read", "cannot read import records")
		}

		for _, r := range recs {
			out[r.ImportId] = r
		}
	}

	return out, nil
}

// ingRecordedTransactionIds returns every transaction id this user's records point at (the
// "recorded" set that fuzzy matching must exclude, §14.6)
func ingRecordedTransactionIds(mc *Ctx) (map[int64]bool, error) {
	db, err := ingDB(mc)

	if err != nil {
		return nil, err
	}

	var recs []*MachineImportRecord

	if err := db.NewSession(mc.Web).Cols("uid", "import_id", "transaction_id").Where("uid=?", mc.Uid).Find(&recs); err != nil {
		errfile.Caught("reading the bound user's import records", err)
		return nil, NewFail(CodeUpstreamError, "check ~/T/ezbookkeeping/error.err; the machine_import_record table could not be read", "cannot read import records")
	}

	out := make(map[int64]bool, len(recs))

	for _, r := range recs {
		out[r.TransactionId] = true
	}

	return out, nil
}

// ingRecordsOfRun returns the records a run wrote
func ingRecordsOfRun(mc *Ctx, runId string) ([]*MachineImportRecord, error) {
	db, err := ingDB(mc)

	if err != nil {
		return nil, err
	}

	var recs []*MachineImportRecord

	if err := db.NewSession(mc.Web).Where("uid=? AND run_id=?", mc.Uid, runId).OrderBy("import_id asc").Find(&recs); err != nil {
		errfile.Caught("reading the import records of a run", err)
		return nil, NewFail(CodeUpstreamError, "check ~/T/ezbookkeeping/error.err; the machine_import_record table could not be read", "cannot read import records")
	}

	return recs, nil
}

// ingTxnState is what the plane needs to know about a transaction row, deleted or not
type ingTxnState struct {
	Id              int64
	Exists          bool
	Deleted         bool
	Type            models.TransactionDbType
	AccountId       int64
	Amount          int64
	UpdatedUnixTime int64
	Comment         string
}

// ingLoadTxnStates reads transactions by id INCLUDING soft-deleted ones (a deletion is a decision
// the plan must see)
func ingLoadTxnStates(mc *Ctx, ids []int64) (map[int64]*ingTxnState, error) {
	out := make(map[int64]*ingTxnState, len(ids))

	if len(ids) == 0 {
		return out, nil
	}

	db, err := ingDB(mc)

	if err != nil {
		return nil, err
	}

	uniq := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))

	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}

	for start := 0; start < len(uniq); start += ingInChunk {
		end := start + ingInChunk

		if end > len(uniq) {
			end = len(uniq)
		}

		var txns []*models.Transaction

		if err := db.NewSession(mc.Web).Cols("transaction_id", "uid", "deleted", "type", "account_id", "amount", "updated_unix_time", "comment").Where("uid=?", mc.Uid).In("transaction_id", uniq[start:end]).Find(&txns); err != nil {
			errfile.Caught("reading the transactions behind the import records", err)
			return nil, NewFail(CodeUpstreamError, "check ~/T/ezbookkeeping/error.err", "cannot read transactions")
		}

		for _, t := range txns {
			out[t.TransactionId] = &ingTxnState{Id: t.TransactionId, Exists: true, Deleted: t.Deleted, Type: t.Type, AccountId: t.AccountId, Amount: t.Amount, UpdatedUnixTime: t.UpdatedUnixTime, Comment: t.Comment}
		}
	}

	for _, id := range uniq {
		if _, ok := out[id]; !ok {
			out[id] = &ingTxnState{Id: id, Exists: false, Deleted: true}
		}
	}

	return out, nil
}

// ingRecordWrite is one change to machine_import_record inside an apply
type ingRecordWrite struct {
	ImportId      string
	TransactionId int64
	AccountKey    string
	SourceFile    string
	// Relink replaces an existing record's transaction id (reimport of a deleted row)
	Relink bool
}

// ingWriteRecords inserts new records and relinks existing ones, in one database transaction
func ingWriteRecords(mc *Ctx, runId string, writes []ingRecordWrite) error {
	if len(writes) == 0 {
		return nil
	}

	db, err := ingDB(mc)

	if err != nil {
		return err
	}

	now := time.Now().Unix()

	err = db.DoTransaction(mc.Web, func(sess *xorm.Session) error {
		for _, w := range writes {
			src := w.SourceFile

			if len(src) > 1024 {
				src = src[len(src)-1024:]
			}

			if w.Relink {
				if _, err := sess.Where("uid=? AND import_id=?", mc.Uid, w.ImportId).Cols("transaction_id", "run_id", "source_file", "created_unix_time").Update(&MachineImportRecord{TransactionId: w.TransactionId, RunId: runId, SourceFile: src, CreatedUnixTime: now}); err != nil {
					return err
				}

				continue
			}

			rec := &MachineImportRecord{Uid: mc.Uid, ImportId: w.ImportId, TransactionId: w.TransactionId, AccountKey: w.AccountKey, SourceFile: src, RunId: runId, CreatedUnixTime: now}

			if _, err := sess.Insert(rec); err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		log.Errorf(mc.Web, "[machine.ingest] cannot write import records for uid %d: %s", mc.Uid, err.Error())
		return NewFail(CodeUpstreamError, "re-run the plan; rows already recorded are skipped (details in ~/T/ezbookkeeping/error.err)", "cannot write import records")
	}

	return nil
}

// ingDeleteRecords removes records by import id, but only while they still point at the expected
// transaction (so an undo never removes a record a later run re-pointed)
func ingDeleteRecords(mc *Ctx, expect map[string]int64) error {
	if len(expect) == 0 {
		return nil
	}

	db, err := ingDB(mc)

	if err != nil {
		return err
	}

	return db.DoTransaction(mc.Web, func(sess *xorm.Session) error {
		for importId, txnId := range expect {
			if _, err := sess.Where("uid=? AND import_id=? AND transaction_id=?", mc.Uid, importId, txnId).Delete(&MachineImportRecord{}); err != nil {
				return err
			}
		}

		return nil
	})
}

// ingRelinkRecords points records back at prior transactions (undo of a reimport)
func ingRelinkRecords(mc *Ctx, prior map[string][2]int64) error {
	if len(prior) == 0 {
		return nil
	}

	db, err := ingDB(mc)

	if err != nil {
		return err
	}

	return db.DoTransaction(mc.Web, func(sess *xorm.Session) error {
		for importId, pair := range prior {
			// pair[0] = the transaction the record points at now, pair[1] = the one to restore
			if _, err := sess.Where("uid=? AND import_id=? AND transaction_id=?", mc.Uid, importId, pair[0]).Cols("transaction_id").Update(&MachineImportRecord{TransactionId: pair[1]}); err != nil {
				return err
			}
		}

		return nil
	})
}
