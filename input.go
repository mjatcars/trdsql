package trdsql

import (
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Reader is wrap the reader.
type Reader interface {
	GetColumn(rowNum int) ([]string, error)
	GetTypes() ([]string, error)
	PreReadRow() [][]interface{}
	ReadRow([]interface{}) ([]interface{}, error)
}

// Import is parses the SQL statement and imports one or more tables.
// Return the rewritten SQL and error.
// No error is returned if there is no table to import.
func (trdsql *TRDSQL) Import(db *DDB, sqlstr string) (string, error) {
	tables := listTable(sqlstr)

	if len(tables) == 0 {
		// without FROM clause. ex. SELECT 1+1;
		debug.Printf("table not found\n")
		return sqlstr, nil
	}

	created := make(map[string]bool)
	for _, fileName := range tables {
		if created[fileName] {
			debug.Printf("already created \"%s\"\n", fileName)
			continue
		}
		tableName, err := trdsql.ImportFile(db, fileName)
		if err != nil {
			return sqlstr, err
		}
		if tableName != "" {
			sqlstr = db.RewriteSQL(sqlstr, fileName, tableName)
			debug.Printf("escaped [%s] -> [%s]\n", fileName, tableName)
		}
		created[fileName] = true
	}

	return sqlstr, nil
}

func listTable(sqlstr string) []string {
	var tables []string
	var tableFlag, frontFlag bool
	word := sqlFields(sqlstr)
	debug.Printf("[%s]", strings.Join(word, "]["))
	for i, w := range word {
		frontFlag = false
		switch {
		case strings.ToUpper(w) == "FROM" || strings.ToUpper(w) == "JOIN":
			tableFlag = true
			frontFlag = true
		case isSQLKeyWords(w):
			tableFlag = false
		case w == ",":
			frontFlag = true
		default:
			frontFlag = false
		}
		if n := i + 1; n < len(word) && tableFlag && frontFlag {
			if t := word[n]; len(t) > 0 {
				if t[len(t)-1] == ')' {
					t = t[:len(t)-1]
				}
				if !isSQLKeyWords(t) {
					tables = append(tables, t)
				}
			}
		}
	}
	return tables
}

func sqlFields(line string) []string {
	parsed := []string{}
	buf := ""
	var singleQuoted, doubleQuoted, backQuote bool
	for _, r := range line {
		switch r {
		case ' ', '\t', '\r', '\n', ',', ';', '=':
			if !singleQuoted && !doubleQuoted && !backQuote {
				if buf != "" {
					parsed = append(parsed, buf)
					buf = ""
				}
				if r == ',' {
					parsed = append(parsed, ",")
				}
			} else {
				buf += string(r)
			}
			continue
		case '\'':
			if !doubleQuoted && !backQuote {
				singleQuoted = !singleQuoted
			}
		case '"':
			if !singleQuoted && !backQuote {
				doubleQuoted = !doubleQuoted
			}
		case '`':
			if !singleQuoted && !doubleQuoted {
				backQuote = !backQuote
			}
		}
		buf += string(r)
	}
	parsed = append(parsed, buf)
	return parsed
}

func isSQLKeyWords(str string) bool {
	switch strings.ToUpper(str) {
	case "WHERE", "GROUP", "HAVING", "WINDOW", "UNION", "ORDER", "LIMIT", "OFFSET", "FETCH",
		"FOR", "LEFT", "RIGHT", "CROSS", "INNER", "FULL", "LETERAL", "(SELECT":
		return true
	}
	return false
}

// ImportFile is imports a file.
// Return the escaped table name and error.
// Do not import if file not found (no error)
func (trdsql *TRDSQL) ImportFile(db *DDB, fileName string) (string, error) {
	file, err := trdsql.importFileOpen(fileName)
	if err != nil {
		debug.Printf("%s\n", err)
		return "", nil
	}
	defer file.Close()
	reader, err := trdsql.NewReader(file, fileName)
	if err != nil {
		return "", err
	}

	if DefaultReadOpts.InSkip > 0 {
		skip := make([]interface{}, 1)
		for i := 0; i < DefaultReadOpts.InSkip; i++ {
			r, e := reader.ReadRow(skip)
			if e != nil {
				log.Printf("ERROR: skip error %s", e)
				break
			}
			debug.Printf("Skip row:%s\n", r)
		}
	}
	tableName := db.EscapeTable(fileName)
	columnNames, err := reader.GetColumn(DefaultReadOpts.InPreRead)
	if err != nil {
		if err != io.EOF {
			return tableName, err
		}
		debug.Printf("EOF reached before argument number of rows")
	}
	columnTypes, err := reader.GetTypes()
	if err != nil {
		if err != io.EOF {
			return tableName, err
		}
		debug.Printf("EOF reached before argument number of rows")
	}
	debug.Printf("Column Names: [%v]", strings.Join(columnNames, ","))
	debug.Printf("Column Types: [%v]", strings.Join(columnTypes, ","))

	err = db.CreateTable(tableName, columnNames, columnTypes)
	if err != nil {
		return tableName, err
	}
	err = db.Import(tableName, columnNames, reader, DefaultReadOpts.InPreRead)
	return tableName, err
}

// NewReader returns an Reader interface
// depending on the file to be imported.
func (trdsql *TRDSQL) NewReader(reader io.Reader, fileName string) (Reader, error) {
	if DefaultReadOpts.InFormat == GUESS {
		DefaultReadOpts.InFormat = guessExtension(fileName)
	}

	switch DefaultReadOpts.InFormat {
	case CSV:
		return NewCSVReader(reader, DefaultReadOpts)
	case LTSV:
		return NewLTSVReader(reader)
	case JSON:
		return NewJSONReader(reader)
	case TBLN:
		return NewTBLNReader(reader)
	default:
		return nil, fmt.Errorf("unknown formatt")
	}
}

func guessExtension(tableName string) Format {
	if strings.HasSuffix(tableName, ".gz") {
		tableName = tableName[0 : len(tableName)-3]
	}
	pos := strings.LastIndex(tableName, ".")
	if pos == 0 {
		debug.Printf("Set in CSV because the extension is unknown: [%s]", tableName)
		return CSV
	}
	ext := strings.ToUpper(tableName[pos+1:])
	switch ext {
	case "CSV":
		debug.Printf("Guess file type as CSV: [%s]", tableName)
		return CSV
	case "LTSV":
		debug.Printf("Guess file type as LTSV: [%s]", tableName)
		return LTSV
	case "JSON":
		debug.Printf("Guess file type as JSON: [%s]", tableName)
		return JSON
	case "TBLN":
		debug.Printf("Guess file type as TBLN: [%s]", tableName)
		return TBLN
	default:
		debug.Printf("Set in CSV because the extension is unknown: [%s]", tableName)
		return CSV
	}
}

func (trdsql *TRDSQL) importFileOpen(tableName string) (io.ReadCloser, error) {
	r := regexp.MustCompile(`\*|\?|\[`)
	if r.MatchString(tableName) {
		return globFileOpen(tableName)
	}
	return tableFileOpen(tableName)
}

func tableFileOpen(fileName string) (io.ReadCloser, error) {
	if len(fileName) == 0 || fileName == "-" || strings.ToLower(fileName) == "stdin" {
		return os.Stdin, nil
	}
	fileName = trimQuote(fileName)
	file, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	return extFileReader(fileName, file), nil
}

func globFileOpen(fileName string) (*io.PipeReader, error) {
	fileName = trimQuote(fileName)
	files, err := filepath.Glob(fileName)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no matches found: %s", fileName)
	}
	pipeReader, pipeWriter := io.Pipe()
	go func() {
		defer pipeWriter.Close()
		for _, file := range files {
			f, err := os.Open(file)
			debug.Printf("Open: [%s]", file)
			if err != nil {
				log.Printf("ERROR: %s:%s", file, err)
				continue
			}
			r := extFileReader(file, f)
			_, err = io.Copy(pipeWriter, r)
			if err != nil {
				log.Printf("ERROR: %s:%s", file, err)
				continue
			}
			_, err = pipeWriter.Write([]byte("\n"))
			if err != nil {
				log.Printf("ERROR: %s:%s", file, err)
				continue
			}
			err = f.Close()
			if err != nil {
				log.Printf("ERROR: %s:%s", file, err)
			}
		}
	}()
	return pipeReader, nil
}

func trimQuote(fileName string) string {
	if fileName[0] == '`' {
		fileName = strings.Replace(fileName, "`", "", 2)
	}
	if fileName[0] == '"' {
		fileName = strings.Replace(fileName, "\"", "", 2)
	}
	return fileName
}

func extFileReader(fileName string, reader *os.File) io.ReadCloser {
	if strings.HasSuffix(fileName, ".gz") {
		z, err := gzip.NewReader(reader)
		if err != nil {
			debug.Printf("No gzip file: [%s]", fileName)
			_, err := reader.Seek(0, io.SeekStart)
			if err != nil {
				return nil
			}
			return reader
		}
		debug.Printf("decompress gzip file: [%s]", fileName)
		return z
	}
	return reader
}
