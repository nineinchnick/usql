// Package trino defines and registers usql's Trino driver.
//
// See: https://github.com/trinodb/trino-go-client
package trino

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	_ "github.com/trinodb/trino-go-client/trino" // DRIVER
	"github.com/xo/tblfmt"
	"github.com/xo/usql/drivers"
	"github.com/xo/usql/drivers/metadata"
	infos "github.com/xo/usql/drivers/metadata/informationschema"
	"github.com/xo/usql/env"
)

func init() {
	endRE := regexp.MustCompile(`;?\s*$`)
	newReader := infos.New(
		infos.WithPlaceholder(func(int) string { return "?" }),
		infos.WithCustomClauses(map[infos.ClauseName]string{
			infos.ColumnsColumnSize:       "0",
			infos.ColumnsNumericScale:     "0",
			infos.ColumnsNumericPrecRadix: "0",
			infos.ColumnsCharOctetLength:  "0",
		}),
		infos.WithFunctions(false),
		infos.WithSequences(false),
		infos.WithIndexes(false),
		infos.WithConstraints(false),
	)
	drivers.Register("trino", drivers.Driver{
		AllowMultilineComments: true,
		Process: func(prefix string, sqlstr string) (string, string, bool, error) {
			sqlstr = endRE.ReplaceAllString(sqlstr, "")
			typ, q := drivers.QueryExecType(prefix, sqlstr)
			return typ, sqlstr, q, nil
		},
		Version: func(ctx context.Context, db drivers.DB) (string, error) {
			var ver string
			err := db.QueryRowContext(
				ctx,
				`SELECT node_version FROM system.runtime.nodes LIMIT 1`,
			).Scan(&ver)
			if err != nil {
				return "", err
			}
			return "Trino " + ver, nil
		},
		NewMetadataReader: newReader,
		NewMetadataWriter: func(db drivers.DB, w io.Writer, opts ...metadata.ReaderOption) metadata.Writer {
			writerOpts := []metadata.WriterOption{
				metadata.WithListAllDbs(func(pattern string, verbose bool) error {
					return listAllDbs(db, w, pattern, verbose)
				}),
			}
			return metadata.NewDefaultWriter(newReader(db, opts...), writerOpts...)(db, w)
		},
		Copy: drivers.CopyWithInsert(func(int) string { return "?" }),
		StatsQuery: func(ctx context.Context, db drivers.DB, name, pattern string, k int, percentiles []float64) (string, error) {
			if !strings.ContainsAny(pattern, "mspk") {
				if strings.ContainsRune(name, ' ') {
					name = "(" + name + ")"
				}
				return "SHOW STATS FOR " + name, nil
			}
			rows, err := db.QueryContext(ctx, name)
			if err != nil {
				return "", err
			}
			cols, err := rows.ColumnTypes()
			if err != nil {
				return "", err
			}
			err = rows.Close()
			if err != nil {
				return "", err
			}
			unions := []string{}
			for _, c := range cols {
				isNum := false
				switch strings.ToLower(c.DatabaseTypeName()) {
				case "int", "bigint", "float", "double", "decimal", "timestamp":
					isNum = true
				}

				stats := []string{
					fmt.Sprintf(`'%s' AS "column"`, c.Name()),
					fmt.Sprintf(`'%s' AS "type"`, c.Name()),
				}
				if strings.ContainsRune(pattern, 'c') {
					stats = append(stats,
						`count(*) AS "count"`,
						fmt.Sprintf(`count(%s) AS "nonempty"`, c.Name()),
					)
				}
				if strings.ContainsRune(pattern, 'm') {
					if isNum {
						stats = append(stats, fmt.Sprintf(`avg(%s) AS "mean"`, c.Name()))
					} else {
						stats = append(stats, `0 AS "mean"`)
					}
				}
				if strings.ContainsRune(pattern, 's') {
					if isNum {
						stats = append(stats, fmt.Sprintf(`stddev(%s) AS "std"`, c.Name()))
					} else {
						stats = append(stats, `0 AS "std"`)
					}
				}
				if strings.ContainsRune(pattern, 'l') {
					stats = append(stats, fmt.Sprintf(`cast(min(%s) AS varchar) AS "low"`, c.Name()))
				}
				if strings.ContainsRune(pattern, 'h') {
					stats = append(stats, fmt.Sprintf(`cast(max(%s) AS varchar) AS "high"`, c.Name()))
				}
				if strings.ContainsRune(pattern, 'u') {
					stats = append(stats, fmt.Sprintf(`count(distinct %s) AS "unique"`, c.Name()))
				}
				if strings.ContainsRune(pattern, 'p') {
					for _, p := range percentiles {
						label := "q" + fmt.Sprintf("%.2f", p)[2:]
						if !isNum {
							stats = append(stats, fmt.Sprintf(`'' AS "%s"`, label))
							continue
						}
						stats = append(stats, fmt.Sprintf(`cast(approx_percentile('%s', %f) AS varchar) AS "%s"`, c.Name(), p, label))
					}
				}
				if strings.ContainsRune(pattern, 'k') && k > 0 {
					stats = append(stats, fmt.Sprintf("map_keys(approx_most_frequent(%d, cast(%s AS varchar), 1000)) AS top%d", k, c.Name(), k))
					stats = append(stats, fmt.Sprintf("map_values(approx_most_frequent(%d, cast(%s AS varchar), 1000)) AS freq%d", k, c.Name(), k))
				}
				unions = append(unions, fmt.Sprintf("SELECT %s FROM (%s) a", strings.Join(stats, ", "), name))
			}
			return strings.Join(unions, " UNION ALL "), nil
		},
	})
}

func listAllDbs(db drivers.DB, w io.Writer, pattern string, verbose bool) error {
	rows, err := db.Query("SHOW catalogs")
	if err != nil {
		return err
	}
	defer rows.Close()

	params := env.Pall()
	params["title"] = "List of databases"
	return tblfmt.EncodeAll(w, rows, params)
}
