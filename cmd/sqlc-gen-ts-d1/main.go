package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/orisano/sqlc-gen-ts-d1/codegen/plugin"
)

// handler returns generated code information based on schema and query information parsed by sqlc
func handler(request *plugin.CodeGenRequest) (*plugin.CodeGenResponse, error) {
	options, err := parseOption(request.GetPluginOptions())
	if err != nil {
		return nil, fmt.Errorf("parse option: %w", err)
	}
	workersTypesVersion := "2022-11-30"
	if v, ok := options["workers-types"]; ok {
		workersTypesVersion = v
	}
	workersTypesV3 := false
	if v, ok := options["workers-types-v3"]; ok {
		workersTypesV3 = v == "1"
	}

	tsTypeMap := buildTsTypeMap(request.GetSettings())
	var files []*plugin.File
	{
		// Schema types are needed for sqlc.embed, so output as models.ts
		models := bytes.NewBuffer(nil)
		appendMeta(models, request)
		for _, s := range request.GetCatalog().GetSchemas() {
			for _, t := range s.GetTables() {
				modelName := naming.toModelTypeName(t.GetRel())
				fmt.Fprintf(models, "export type %s = {\n", modelName)
				for _, c := range t.GetColumns() {
					colName := naming.toPropertyName(c)
					tsType := tsTypeMap.toTsType(c)
					fmt.Fprintf(models, "  %s: %s;\n", colName, tsType)
				}
				fmt.Fprintf(models, "};\n\n")
			}
		}
		files = append(files, &plugin.File{Name: "models.ts", Contents: models.Bytes()})
	}

	{
		querier := bytes.NewBuffer(nil)

		tableMap := buildTableMap(request.GetCatalog())

		workersTypesPackage := "@cloudflare/workers-types"
		if workersTypesVersion != "" {
			workersTypesPackage += "/" + workersTypesVersion
		}

		header := bytes.NewBuffer(nil)
		appendMeta(header, request)
		if !workersTypesV3 {
			header.WriteString("import { D1Database, D1PreparedStatement, D1Result } from \"" + workersTypesPackage + "\"\n")
		}

		querier.WriteString("type Query<T> = {\n")
		querier.WriteString("  then(onFulfilled?: (value: T) => void, onRejected?: (reason?: any) => void): void;\n")
		querier.WriteString("  batch(): D1PreparedStatement;\n")
		querier.WriteString("}\n")

		requireModels := map[string]bool{}
		requireExpandedParams := false

		for _, q := range request.GetQueries() {
			queryText := q.GetText()
			// sqlc.embed expands columns in the form x.a, x.b, x.c
			// Some systems cannot obtain info about duplicate-named columns after expanding multiple sqlc.embed
			// Therefore, avoid the problem by rewriting the query as x.a AS x_a, x.b AS x_b, x.c AS x_c
			// When rewriting one column at a time, prefix/suffix matching must be considered, so rewrite all at once
			for _, c := range q.GetColumns() {
				et := c.GetEmbedTable()
				if et.GetName() == "" {
					continue
				}
				var news, olds []string
				for _, ec := range tableMap.findTable(et).GetColumns() {
					from := et.GetName() + "." + ec.GetName()
					to := from + " AS " + naming.toEmbedColumnName(et, ec)
					olds = append(olds, from)
					news = append(news, to)
				}
				queryText = strings.Replace(queryText, strings.Join(olds, ", "), strings.Join(news, ", "), 1)
			}

			query := "-- name: " + q.GetName() + " " + q.GetCmd() + "\n" + queryText
			fmt.Fprintf(querier, "const %s = `%s`;\n", naming.toConstQueryName(q), query)

			querier.WriteByte('\n')

			// Do not generate type when there are 0 parameters, as they will be removed from arguments
			if len(q.GetParams()) > 0 {
				fmt.Fprintf(querier, "export type %s = {\n", naming.toParamsTypeName(q))
				for _, p := range q.GetParams() {
					c := p.GetColumn()
					paramName := naming.toPropertyName(c)
					tsType := tsTypeMap.toTsType(c)
					// Parameters are nullable only when using sqlc.narg
					if c.GetNotNull() {
						// If the corresponding column is known and nullable in the schema, make the parameter nullable
						if tc := tableMap.findColumn(c); tc != nil && !tc.GetNotNull() {
							tsType += " | null"
						}
					}
					fmt.Fprintf(querier, "  %s: %s;\n", paramName, tsType)
				}
				querier.WriteString("};\n")

				querier.WriteByte('\n')
			}

			needRawType := false
			// :exec does not return a response, so do not generate a type
			if q.GetCmd() != ":exec" {
				fmt.Fprintf(querier, "export type %s = {\n", naming.toQueryRowTypeName(q))
				for _, c := range q.GetColumns() {
					colName := c.GetName()
					propName := naming.toPropertyName(c)

				// When column name (snake) and property name (camel) differ
				// An internal result type is needed because conversion is required within the generated code
					if colName != propName {
						needRawType = true
					}

					tsType := ""

				// When sqlc.embed is used
				// An internal result type is needed because conversion is required within the generated code
					if et := c.GetEmbedTable(); et.GetName() != "" {
						needRawType = true
						tsType = naming.toModelTypeName(et)
						// Import from models.ts is required
						requireModels[tsType] = true
					} else {
						tsType = tsTypeMap.toTsType(c)
					}
					fmt.Fprintf(querier, "  %s: %s;\n", propName, tsType)
				}
				querier.WriteString("};\n")

				querier.WriteByte('\n')
			}

			// Generate only when internal result type is needed
			if needRawType {
				fmt.Fprintf(querier, "type %s = {\n", naming.toRawQueryRowTypeName(q))
				for _, c := range q.GetColumns() {
					// In case of sqlc.embed, get column info from schema and expand
					if et := c.GetEmbedTable(); et.GetName() != "" {
						for _, ec := range tableMap.findTable(et).GetColumns() {
							colName := naming.toEmbedColumnName(et, ec)
							tsType := tsTypeMap.toTsType(ec)
							fmt.Fprintf(querier, "  %s: %s;\n", colName, tsType)
						}
					} else {
						colName := c.GetName()
						tsType := tsTypeMap.toTsType(c)
						fmt.Fprintf(querier, "  %s: %s;\n", colName, tsType)
					}
				}
				querier.WriteString("};\n")

				querier.WriteByte('\n')
			}

			rowType := naming.toQueryRowTypeName(q)
			// retType is the return type of the function
			var retType string
			// resultType is the return type from SQLite
			var resultType string

			if cmd := q.GetCmd(); cmd == ":one" {
				retType = rowType + " | null"
				resultType = retType
				if needRawType {
					resultType = naming.toRawQueryRowTypeName(q) + " | null"
				}
			} else if cmd == ":exec" {
				retType = "D1Result"
			} else {
				retType = "D1Result<" + rowType + ">"
				resultType = rowType
				if needRawType {
					resultType = naming.toRawQueryRowTypeName(q)
				}
			}

			fmt.Fprintf(querier, "export function %s(\n", naming.toFunctionName(q))
			fmt.Fprintf(querier, "  d1: D1Database")
			// Do not add arguments when there are no parameters
			if len(q.GetParams()) > 0 {
				querier.WriteString(",\n")
				fmt.Fprintf(querier, "  args: %s", naming.toParamsTypeName(q))
			}
			querier.WriteString("\n")
			fmt.Fprintf(querier, "): Query<%s> {\n", retType)

			var queryVar string
			var bindArgs string
			if hasSqlcSlice(q) {
				// SQLite cannot specify arrays as parameters, so sqlc.slice requires query rewriting at runtime
				// sqlc auto-numbers parameters, so sqlc.slice parameters are numbered in order of appearance
				// However, the ? outputs a string without a number (sqlc-dev/sqlc/pull/2274)
				// The number of parameters changes dynamically, but existing parameter numbers should not be rewritten, so pass the first element as-is and append dynamic parameters to the end
				// Example:
				//  Query:
				//    SELECT * FROM foo WHERE a = @a AND id IN (sqlc.slice(ids)) AND b = @b
				//  Compiled:
				//    SELECT id, a, b FROM foo WHERE a = ?1 AND id IN (/*SLICE:ids*/?) AND b = ?3
				//  Runtime (ids has length 3):
				//    SELECT id, a, b FROM foo WHERE a = ?1 AND id IN (?2, ?4, ?5) AND b = ?3
				fmt.Fprintf(querier, "  let query = %s;\n", naming.toConstQueryName(q))
				fmt.Fprintf(querier, "  const params: any[] = [%s];\n", buildBindArgs(q))
				for _, p := range q.GetParams() {
					c := p.GetColumn()
					if !c.GetIsSqlcSlice() {
						continue
					}
					n := p.GetNumber()
					propName := naming.toPropertyName(c)
					// sqlc.slice outputs the query in the format (/*SLICE:foo*/?) (sqlc-dev/sqlc/pull/2274)
					// Rewrite in the form (?1, ?2, ?3)
					fmt.Fprintf(querier, "  query = query.replace(\"(/*SLICE:%s*/?)\", expandedParam(%d, args.%s.length, params.length));\n", c.Name, n, propName)
					// The first element is included in params at declaration time, so push the rest
					fmt.Fprintf(querier, "  params.push(...args.%s.slice(1));\n", propName)
				}
				queryVar = "query"
				bindArgs = "...params"
				requireExpandedParams = true
			} else {
				queryVar = naming.toConstQueryName(q)
				bindArgs = buildBindArgs(q)
			}

			fmt.Fprintf(querier, "  const ps = d1\n")
			fmt.Fprintf(querier, "    .prepare(%s)", queryVar)
			if len(q.GetParams()) > 0 {
				querier.WriteString("\n")
				fmt.Fprintf(querier, "    .bind(%s)", bindArgs)
			}
			querier.WriteString(";\n")

			fmt.Fprintf(querier, "  return {\n")
			fmt.Fprintf(querier, "    then(onFulfilled?: (value: %s) => void, onRejected?: (reason?: any) => void) {\n", retType)

			switch q.GetCmd() {
			case ":one":
				fmt.Fprintf(querier, "      ps.first<%s>()\n", resultType)
			case ":many":
				fmt.Fprintf(querier, "      ps.all<%s>()\n", resultType)
			case ":exec":
				fmt.Fprintf(querier, "      ps.run()\n")
			}

			// When using internal result type, generate conversion to result type
			if needRawType {
				if q.GetCmd() == ":one" {
					fmt.Fprintf(querier, "        .then((raw: %s) => raw ? {\n", resultType)
					writeFromRawMapping(querier, "          ", tableMap, q)
					fmt.Fprintf(querier, "        } : null)\n")
				} else {
					fmt.Fprintf(querier, "        .then((r: D1Result<%s>) => { return {\n", resultType)
					fmt.Fprintf(querier, "          ...r,\n")
					if workersTypesV3 {
						fmt.Fprintf(querier, "          results: r.results ? r.results.map((raw: %s) => { return {\n", resultType)
						writeFromRawMapping(querier, "             ", tableMap, q)
						fmt.Fprintf(querier, "          }}) : undefined,\n")
					} else {
						fmt.Fprintf(querier, "          results: r.results.map((raw: %s) => { return {\n", resultType)
						writeFromRawMapping(querier, "            ", tableMap, q)
						fmt.Fprintf(querier, "          }}),\n")
					}
					fmt.Fprintf(querier, "        }})\n")
				}
			}
			fmt.Fprintf(querier, "        .then(onFulfilled).catch(onRejected);\n")
			fmt.Fprintf(querier, "    },\n")
			fmt.Fprintf(querier, "    batch() { return ps; },\n")
			fmt.Fprintf(querier, "  }\n")
			querier.WriteString("}\n")

			querier.WriteByte('\n')
		}

		if requireExpandedParams {
			// Function used when sqlc.slice requires query rewriting at runtime
			querier.WriteString(`function expandedParam(n: number, len: number, last: number): string {
  const params: number[] = [n];
  for (let i = 1; i < len; i++) {
    params.push(last + i);
  }
  return "(" + params.map((x: number) => "?" + x).join(", ") + ")";
}
`)
		}

		if len(requireModels) > 0 {
			var models []string
			for k := range requireModels {
				models = append(models, k)
			}
			sort.Strings(models)
			fmt.Fprintf(header, "import { %s } from \"./models\"\n", strings.Join(models, ", "))
		}
		if header.Len() > 0 {
			header.WriteString("\n")
		}
		files = append(files, &plugin.File{Name: "querier.ts", Contents: append(header.Bytes(), querier.Bytes()...)})
	}

	return &plugin.CodeGenResponse{
		Files: files,
	}, nil
}

// TableMap is a map that allows searching table information in the schema
type TableMap struct {
	m map[string]*tableMapEntry
}

func (m *TableMap) findColumn(c *plugin.Column) *plugin.Column {
	t := c.GetTable()
	if t == nil {
		return nil
	}
	table := m.m[t.GetName()]
	if table == nil {
		return nil
	}
	return table.m[c.GetName()]
}

func (m *TableMap) findTable(table *plugin.Identifier) *plugin.Table {
	t := m.m[table.GetName()]
	if t == nil {
		return nil
	}
	return t.t
}

type tableMapEntry struct {
	t *plugin.Table
	m map[string]*plugin.Column
}

func buildTableMap(catalog *plugin.Catalog) TableMap {
	tm := TableMap{
		m: map[string]*tableMapEntry{},
	}
	for _, schema := range catalog.GetSchemas() {
		for _, table := range schema.GetTables() {
			e := tableMapEntry{
				t: table,
				m: map[string]*plugin.Column{},
			}
			for _, column := range table.GetColumns() {
				e.m[column.GetName()] = column
			}
			tm.m[table.GetRel().GetName()] = &e
		}
	}
	return tm
}

// parseOption takes a quoted comma-separated `key=value` format or JSON object format input and returns it as a map
// Example: `"foo1=bar,foo2=buz"` => map[string]string{"foo1": "bar", "foo2": "buz"}
// Example: `{"foo1":"bar","foo2":"buz"}` => map[string]string{"foo1": "bar", "foo2": "buz"}
func parseOption(opt []byte) (map[string]string, error) {
	m := map[string]string{}
	if len(opt) == 0 {
		return m, nil
	}

	if bytes.HasPrefix(opt, []byte("{")) {
		if err := json.Unmarshal(opt, &m); err != nil {
			return nil, fmt.Errorf("unmarshal: %w", err)
		}
		return m, nil
	}

	s, _ := strconv.Unquote(string(opt))
	for _, kv := range strings.Split(s, ",") {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m, nil
}

type TsTypeMap struct {
	m map[string]string
}

func (t *TsTypeMap) toTsType(col *plugin.Column) string {
	dbType := col.GetType().GetName()
	tsType, ok := t.m[strings.ToUpper(dbType)]
	if !ok {
		tsType = "number | string"
	}
	if col.GetIsSqlcSlice() {
		tsType += "[]"
	}
	if !col.GetNotNull() {
		tsType += " | null"
	}
	return tsType
}

func buildTsTypeMap(settings *plugin.Settings) *TsTypeMap {
	// https://developers.cloudflare.com/d1/platform/client-api/#type-conversion
	m := map[string]string{
		"NULL":     "null",
		"REAL":     "number",
		"INTEGER":  "number",
		"TEXT":     "string",
		"DATETIME": "string",
		"JSON":     "string",
		"BLOB":     "ArrayBuffer",
	}
	for _, o := range settings.GetOverrides() {
		m[strings.ToUpper(o.GetDbType())] = o.GetCodeType()
	}
	return &TsTypeMap{m: m}
}

func toUpperCamel(snake string) string {
	var b strings.Builder
	for _, t := range strings.Split(snake, "_") {
		if t != "" {
			b.WriteString(strings.ToUpper(t[:1]) + t[1:])
		}
	}
	return b.String()
}

func toLowerCamel(snake string) string {
	s := toUpperCamel(snake)
	return strings.ToLower(s[:1]) + s[1:]
}

type Naming struct{}

// toModelTypeName returns the model type name output to models.ts
func (Naming) toModelTypeName(table *plugin.Identifier) string {
	return toUpperCamel(table.GetName())
}

// toPropertyName returns the TypeScript property name
func (Naming) toPropertyName(col *plugin.Column) string {
	return col.GetName()
}

// toConstQueryName returns the constant name of the query string
func (Naming) toConstQueryName(q *plugin.Query) string {
	return toLowerCamel(q.GetName()) + "Query"
}

// toParamsTypeName returns the parameter type name of the query
func (Naming) toParamsTypeName(q *plugin.Query) string {
	return q.GetName() + "Params"
}

// toQueryRowTypeName returns the result type name of the query
func (Naming) toQueryRowTypeName(q *plugin.Query) string {
	return q.GetName() + "Row"
}

// toRawQueryRowTypeName returns the internal result type name of the query
func (Naming) toRawQueryRowTypeName(q *plugin.Query) string {
	return "Raw" + q.GetName() + "Row"
}

// toEmbedColumnName returns the column name when sqlc.embed is used
func (Naming) toEmbedColumnName(e *plugin.Identifier, c *plugin.Column) string {
	// MEMO: A single "_" could potentially collide with other column names
	return e.GetName() + "_" + c.GetName()
}

// toFunctionName returns the function name of the query function
func (Naming) toFunctionName(q *plugin.Query) string {
	return q.GetName()
}

var naming Naming

func hasSqlcSlice(q *plugin.Query) bool {
	for _, p := range q.GetParams() {
		if p.GetColumn().GetIsSqlcSlice() {
			return true
		}
	}
	return false
}

func buildBindArgs(q *plugin.Query) string {
	var args strings.Builder
	for i, p := range q.GetParams() {
		if i > 0 {
			args.WriteString(", ")
		}
		args.WriteString("args." + naming.toPropertyName(p.GetColumn()))
		if p.GetColumn().GetIsSqlcSlice() {
			args.WriteString("[0]")
		}
	}
	return args.String()
}

func writeFromRawMapping(w *bytes.Buffer, indent string, tableMap TableMap, q *plugin.Query) {
	for _, c := range q.GetColumns() {
		propName := naming.toPropertyName(c)
		// In case of sqlc.embed, convert to model type
		if et := c.GetEmbedTable(); et.GetName() != "" {
			fmt.Fprintf(w, "%s// sqlc.embed(%s)\n", indent, propName)
			fmt.Fprintf(w, "%s%s: {\n", indent, propName)
			for _, ec := range tableMap.findTable(et).GetColumns() {
				from := naming.toEmbedColumnName(et, ec)
				to := naming.toPropertyName(ec)
				fmt.Fprintf(w, "%s  %s: raw.%s,\n", indent, to, from)
			}
			fmt.Fprintf(w, "%s},\n", indent)
		} else {
			from := c.GetName()
			fmt.Fprintf(w, "%s%s: raw.%s,\n", indent, propName, from)
		}
	}
}

type Handler func(*plugin.CodeGenRequest) (*plugin.CodeGenResponse, error)

func run(h Handler) error {
	var req plugin.CodeGenRequest
	reqBlob, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	if err := req.UnmarshalVT(reqBlob); err != nil {
		return err
	}
	resp, err := h(&req)
	if err != nil {
		return err
	}
	respBlob, err := resp.MarshalVT()
	if err != nil {
		return err
	}
	w := bufio.NewWriter(os.Stdout)
	if _, err := w.Write(respBlob); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return nil
}

var version string
var revision string

func appendMeta(b *bytes.Buffer, req *plugin.CodeGenRequest) {
	v := "v0.0.0-a"
	if version != "" {
		v = version
	}
	r := "HEAD"
	if revision != "" {
		r = revision
	}
	sha256 := req.GetSettings().GetCodegen().GetWasm().GetSha256()
	if sha256 != "" {
		r = sha256
	}
	b.WriteString("// Code generated by sqlc-gen-ts-d1. DO NOT EDIT.\n")
	b.WriteString("// versions:\n")
	fmt.Fprintf(b, "//   sqlc %s\n", req.SqlcVersion)
	fmt.Fprintf(b, "//   sqlc-gen-ts-d1 %s@%s\n", v, r)
	b.WriteString("\n")
}

func main() {
	if err := run(handler); err != nil {
		fmt.Fprintf(os.Stderr, "error generating output: %s", err)
		os.Exit(2)
	}
}
