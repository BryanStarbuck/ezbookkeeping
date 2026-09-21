package machine

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mayswind/ezbookkeeping/pkg/errfile"
	"github.com/mayswind/ezbookkeeping/pkg/models"
)

// ingest_file.go — the browser's Import dialog for one file (apis.mdx §14.4 /ingest/file/plan and
// /ingest/file/apply; cli.mdx §10.9 `ezbk statements import-file`): the same plan and apply,
// scoped to one file and one account, parsed by upstream's converter, de-duplicated against
// machine_import_record.

// ingFileArgs are the arguments of /ingest/file/plan and /ingest/file/apply
type ingFileArgs struct {
	AccountId           string            `json:"account_id,omitempty"`
	AccountName         string            `json:"account_name,omitempty"`
	AccountKey          string            `json:"account_key,omitempty"`
	Root                string            `json:"root,omitempty"`
	Staging             string            `json:"staging,omitempty"`
	Path                string            `json:"path,omitempty"`
	FileName            string            `json:"file_name,omitempty"`
	FileType            string            `json:"file_type,omitempty"`
	ColumnMap           *ingCustomOpts    `json:"column_map,omitempty"`
	Start               string            `json:"start,omitempty"`
	End                 string            `json:"end,omitempty"`
	CategoryMap         map[string]string `json:"category_map,omitempty"`
	FallbackCategoryIds map[string]string `json:"fallback_category_ids,omitempty"`
	AcceptMatches       []json.RawMessage `json:"accept_matches,omitempty"`
	ReimportDeleted     bool              `json:"reimport_deleted,omitempty"`
	IncludeRows         bool              `json:"include_rows,omitempty"`
	RowsLimit           int               `json:"rows_limit,omitempty"`
}

// ingFileApplyBody is the JSON body of /ingest/file/apply
type ingFileApplyBody struct {
	ingFileArgs
	WriteOpts
}

// ingFileRequest reads the file arguments from JSON or from a multipart form (field "file" plus
// the same argument names; map arguments as JSON strings)
func ingFileRequest(mc *Ctx, withWrite bool) (*ingFileArgs, *WriteOpts, []byte, string, error) {
	args := &ingFileArgs{}
	opts := &WriteOpts{}
	ct := strings.ToLower(mc.Gin.GetHeader("Content-Type"))

	if !strings.HasPrefix(ct, "multipart/") {
		if withWrite {
			body := &ingFileApplyBody{}

			if err := mc.BindBody(body); err != nil {
				return nil, nil, nil, "", err
			}

			*args, *opts = body.ingFileArgs, body.WriteOpts
		} else if err := mc.BindBody(args); err != nil {
			return nil, nil, nil, "", err
		}

		return args, opts, nil, "", nil
	}

	if err := mc.Gin.Request.ParseMultipartForm(MaxBodyBytes); err != nil {
		errfile.Expected("parsing the multipart form of the upload", err)
		return nil, nil, nil, "", Invalid("send multipart/form-data with the statement in the field \"file\" (at most 16 MiB)", "the multipart form cannot be read")
	}

	form := mc.Gin.Request.MultipartForm
	known := map[string]bool{
		"account_id": true, "account_name": true, "account_key": true, "root": true, "staging": true, "path": true, "file_name": true, "file_type": true,
		"column_map": true, "start": true, "end": true, "category_map": true, "fallback_category_ids": true, "accept_matches": true,
		"reimport_deleted": true, "include_rows": true, "rows_limit": true,
	}

	if withWrite {
		for _, k := range []string{"dry_run", "confirm_token", "max_changes", "idempotency_key"} {
			known[k] = true
		}
	}

	for k := range form.Value {
		if !known[k] {
			return nil, nil, nil, "", Invalid("remove the field, or check its spelling against GET /machine/v1/capabilities", "unknown argument %s", k)
		}
	}

	get := func(k string) string {
		if v := form.Value[k]; len(v) > 0 {
			return strings.TrimSpace(v[0])
		}

		return ""
	}

	args.AccountId, args.AccountName, args.AccountKey = get("account_id"), get("account_name"), get("account_key")
	args.Root, args.Staging, args.Path, args.FileName, args.FileType = get("root"), get("staging"), get("path"), get("file_name"), get("file_type")
	args.Start, args.End = get("start"), get("end")
	args.ReimportDeleted = get("reimport_deleted") == "true" || get("reimport_deleted") == "1"
	args.IncludeRows = get("include_rows") == "true" || get("include_rows") == "1"
	args.RowsLimit, _ = strconv.Atoi(get("rows_limit"))

	for k, dst := range map[string]any{"column_map": &args.ColumnMap, "category_map": &args.CategoryMap, "fallback_category_ids": &args.FallbackCategoryIds, "accept_matches": &args.AcceptMatches} {
		if v := get(k); v != "" {
			if err := json.Unmarshal([]byte(v), dst); err != nil {
				errfile.Expected("decoding a JSON form field of the upload", err)
				return nil, nil, nil, "", Invalid("pass "+k+" as a JSON string inside the form", "%s is not valid JSON", k)
			}
		}
	}

	if withWrite {
		if v := get("dry_run"); v != "" {
			b := v != "false" && v != "0"
			opts.DryRun = &b
		}

		opts.ConfirmToken = get("confirm_token")
		opts.MaxChanges, _ = strconv.Atoi(get("max_changes"))
		opts.IdempotencyKey = get("idempotency_key")
	}

	var data []byte
	name := ""

	if files := form.File["file"]; len(files) > 0 {
		f, err := files[0].Open()

		if err != nil {
			errfile.Expected("opening the uploaded statement file", err)
			return nil, nil, nil, "", Invalid("attach the statement as the multipart field \"file\"", "the uploaded file cannot be read")
		}

		defer f.Close()

		data, err = io.ReadAll(io.LimitReader(f, MaxBodyBytes+1))

		if err != nil || len(data) > MaxBodyBytes {
			errfile.Expected("reading the uploaded statement file", err)
			return nil, nil, nil, "", Invalid("upload at most 16 MiB, or pass path to a file under the statements root", "the uploaded file is too large")
		}

		name = filepath.Base(files[0].Filename)

		if args.FileName != "" {
			name = filepath.Base(args.FileName)
		}
	}

	return args, opts, data, name, nil
}

// ingResolveFileAccount resolves the target account by id or exact name
func ingResolveFileAccount(accounts *ingAccountIndex, args *ingFileArgs) (*models.Account, error) {
	if args.AccountId != "" {
		id, err := ResolveId("account_id", args.AccountId)

		if err != nil {
			return nil, err
		}

		a := accounts.ById[id]

		if a == nil {
			return nil, NotFound("GET /machine/v1/accounts — or pass account_name instead of account_id", "no account with that id")
		}

		return a, nil
	}

	name := strings.TrimSpace(args.AccountName)

	if name == "" {
		return nil, Invalid("pass account_id (or account_name): the file imports into one account", "account_id is required")
	}

	if exact := accounts.ByName[name]; len(exact) == 1 {
		return exact[0], nil
	} else if len(exact) > 1 {
		return nil, ingAmbiguousAccounts(name, exact)
	}

	var ci []*models.Account

	for _, a := range accounts.All {
		if strings.EqualFold(a.Name, name) {
			ci = append(ci, a)
		}
	}

	if len(ci) == 1 {
		return ci[0], nil
	}

	if len(ci) > 1 {
		return nil, ingAmbiguousAccounts(name, ci)
	}

	return nil, NotFound("GET /machine/v1/accounts lists the names; pass account_id to be exact", "no account named %q", name)
}

func ingAmbiguousAccounts(name string, list []*models.Account) error {
	var cands []map[string]string

	for _, a := range list {
		cands = append(cands, map[string]string{"id": idString(a.AccountId), "name": a.Name, "currency": a.Currency})
	}

	return Invalid("pass account_id to choose one", "%d accounts are named %q", len(list), name).WithDetails(map[string]any{"candidates": cands})
}

// ingPlanFile builds the one-file plan. data/name are the uploaded bytes when the file came in the
// request; otherwise args.Path names a file under the statements root.
func ingPlanFile(mc *Ctx, args *ingFileArgs, data []byte, name string) (*ingPlanResult, []byte, string, error) {
	if err := ingRequireImportAllowed(mc); err != nil {
		return nil, nil, "", err
	}

	accounts, err := ingLoadAccounts(mc)

	if err != nil {
		return nil, nil, "", err
	}

	acct, err := ingResolveFileAccount(accounts, args)

	if err != nil {
		return nil, nil, "", err
	}

	if p := ingMapProblem(acct, ""); p != "" {
		return nil, nil, "", Invalid("import into a visible single account (a parent account holds no transactions)", "%s", p)
	}

	// the statements root is optional for an upload; when it resolves, its map names the account
	var root *ingRoot
	var st *ingStaging
	m := ingNewMap()

	if args.Root != "" || args.Path != "" {
		if root, err = ingResolveRoot(args.Root); err != nil {
			return nil, nil, "", err
		}
	} else if configured, _ := ingConfiguredRoot(); configured != "" {
		root, _ = ingResolveRoot("")
	}

	if root != nil {
		if s, serr := ingResolveStaging(root, args.Staging, ""); serr == nil {
			st = s

			if st.Exists() {
				if mm, _, merr := ingLoadMap(st); merr == nil {
					m = mm
				}
			}
		}
	}

	source := ""

	if data == nil {
		if args.Path == "" {
			return nil, nil, "", Invalid("pass path (relative to the statements root) or upload the file as multipart field \"file\"", "no file given")
		}

		real, err := root.Resolve(args.Path)

		if err != nil {
			return nil, nil, "", err
		}

		if !ingFileExists(real) {
			return nil, nil, "", NotFound("pass path relative to the statements root", "no file at %q", args.Path)
		}

		data, err = ingReadFileBytes(real, int64(mc.Config.MaxImportFileSize)+1)

		if err != nil {
			errfile.Warn("reading the statement file under the statements root", err)
			return nil, nil, "", Invalid("raise [data] max_import_file_size in conf/ezbookkeeping.ini, or split the file", "the file cannot be read or exceeds the import size limit")
		}

		name = filepath.Base(real)
		source = root.Rel(real)
	} else {
		source = "upload:" + name
	}

	if len(data) == 0 {
		return nil, nil, "", Invalid("the file is empty", "no data to import")
	}

	fileType := strings.ToLower(strings.TrimSpace(args.FileType))

	if fileType == "" && args.ColumnMap != nil && args.ColumnMap.FileType != "" {
		fileType = strings.ToLower(args.ColumnMap.FileType)
	}

	if fileType == "" {
		head := string(data)
		_, ft, needs := ingClassifyName(name, func(n int) string {
			if len(head) > n {
				return head[:n]
			}

			return head
		}, "")

		switch {
		case needs == "qif_date_order":
			return nil, nil, "", Invalid("pass file_type qif_ymd, qif_mdy or qif_dmy — a wrong guess silently swaps days and months", "a QIF file needs its date order")
		case needs == "column_map":
			return nil, nil, "", Invalid("pass file_type custom_csv (or custom_tsv, custom_ssv, custom_xlsx, custom_xls) with column_map", "a custom delimited or Excel file needs a column map")
		case ft == "":
			return nil, nil, "", Invalid("pass file_type (GET /machine/v1/ingest/converters lists them)", "the file type of %q cannot be told from its name or content", name)
		}

		fileType = ft
	}

	if strings.HasPrefix(fileType, "ai_") {
		return nil, nil, "", Invalid("pick a converter that reads the file itself; the plane never sends a statement to an LLM (apis.mdx §14.9)", "file type %s is not available on the machine plane", fileType)
	}

	if !ingKnownFileType(fileType) {
		return nil, nil, "", Invalid("GET /machine/v1/ingest/converters lists the file types", "unknown file type %q", fileType)
	}

	key := ingNormKey(args.AccountKey)
	keySource := "argument"

	if key == "" {
		for k, e := range m.Accounts {
			if e.AccountId == idString(acct.AccountId) {
				if key == "" || k < key {
					key = k
				}
			}
		}

		keySource = "map"
	}

	var warnings []string

	if key == "" {
		key = "acct/" + idString(acct.AccountId)
		keySource = "account_id"
		warnings = append(warnings, "this account is not in the statements map, so import ids are keyed on the account id; the same rows arriving later through /ingest/apply under a statement account key would not be recognised — map the account first (PUT /ingest/map) or pass account_key")
	}

	items, perr := ingParseUpstream(mc, data, name, fileType, args.ColumnMap)

	if perr != nil {
		return nil, nil, "", perr
	}

	rows, skipped := ingNormalize(items, ingNormalizeInput{AccountKey: key, SourceFile: source, SourceKind: fileType, FileType: fileType, ExpectedCurrency: acct.Currency})
	in := &ingAccountInput{AccountKey: key, AccountId: acct.AccountId, Currency: acct.Currency, CurrencySource: "account", Converter: fileType, Skipped: skipped, Files: 1, Warnings: warnings}

	if e := m.Accounts[key]; e != nil {
		in.Entity, in.Institution, in.Label, in.Last4, in.Path = e.Entity, e.Institution, e.Label, e.Last4, e.Path
	}

	if refs := ingBankRefs(fileType, data); len(refs) > 0 {
		if ingAlignBankIds(rows, refs) {
			in.BankIds = len(rows)
		} else {
			in.Warnings = append(in.Warnings, "the bank's transaction ids could not be paired one-to-one with the converter's rows; minted ids used")
		}
	}

	in.Statements = []*ingStatement{{File: source, FileType: fileType, Sha: ingSha256(data), Rows: rows}}

	rng, err := ingPlanRange(mc, args.Start, args.End)

	if err != nil {
		return nil, nil, "", err
	}

	res, err := ingEvaluate(mc, []*ingAccountInput{in}, m, ingEngineOpts{Range: rng, CategoryMap: args.CategoryMap, Fallback: args.FallbackCategoryIds, AcceptMatches: args.AcceptMatches, ReimportDeleted: args.ReimportDeleted})

	if err != nil {
		return nil, nil, "", err
	}

	res.Kind = "file"
	res.Mode = "file"
	res.staging = st

	if root != nil {
		res.Root = root.Display
		res.root = root
	}

	if st != nil {
		res.Staging = st.Rel
	}

	res.Warnings = append(res.Warnings, "account key "+key+" (from "+keySource+")")
	ingAttachRows(res, args.IncludeRows, args.RowsLimit)

	return res, data, name, nil
}

// ingKnownFileType reports the converter file types this build has (custom ones included)
func ingKnownFileType(ft string) bool {
	for _, c := range ingConverterCatalog {
		if c.FileType == ft {
			return true
		}
	}

	return false
}

// ingIsUpload reports an upload source label
func ingIsUpload(source string) bool {
	return strings.HasPrefix(source, "upload:")
}
