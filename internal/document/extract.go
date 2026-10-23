package document

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
)

const (
	maxOOXMLDecodedBytes   = 64 << 20
	maxOOXMLExtractedBytes = 24 << 20
)

const (
	docxMediaType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	xlsxMediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	pptxMediaType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
)

var (
	errOOXMLDecodedLimit = errors.New("OOXML decoded-byte limit exceeded")
	errOOXMLExtractLimit = errors.New("OOXML extracted-text limit exceeded")
)

// ExtractOOXMLText extracts readable text from a supported OOXML document.
func ExtractOOXMLText(path string, mediaType string) (string, error) {
	return extractOOXMLTextWithLimit(path, mediaType, maxOOXMLDecodedBytes, maxOOXMLExtractedBytes)
}

// ExtractOOXMLBytes extracts readable text from an in-memory OOXML
// document under the same limits as [ExtractOOXMLText].
func ExtractOOXMLBytes(data []byte, mediaType string) (string, error) {
	if err := checkOOXMLMediaType(mediaType); err != nil {
		return "", err
	}
	packageReader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", NewDocumentError(CodeCorruptOrEncrypted, "The document could not be read.", nil, err)
	}
	return extractOOXMLFiles(packageReader.File, mediaType, maxOOXMLDecodedBytes, maxOOXMLExtractedBytes)
}

// OOXMLMediaType returns the OOXML media type for a .docx, .xlsx or .pptx
// file name, or "" for anything else.
func OOXMLMediaType(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".docx"):
		return docxMediaType
	case strings.HasSuffix(lower, ".xlsx"):
		return xlsxMediaType
	case strings.HasSuffix(lower, ".pptx"):
		return pptxMediaType
	}
	return ""
}

func checkOOXMLMediaType(mediaType string) error {
	switch mediaType {
	case docxMediaType, pptxMediaType, xlsxMediaType:
		return nil
	}
	return NewDocumentError(
		CodeUnsupportedForRoute,
		"The document type cannot be extracted.",
		map[string]any{"media_type": mediaType},
		nil,
	)
}

// extractOOXMLTextWithLimit keeps extraction bounded while mapping bad packages and size overruns to stable document errors.
func extractOOXMLTextWithLimit(path string, mediaType string, maxDecoded, maxOut int64) (string, error) {
	if err := checkOOXMLMediaType(mediaType); err != nil {
		return "", err
	}
	packageReader, err := zip.OpenReader(path)
	if err != nil {
		return "", NewDocumentError(CodeCorruptOrEncrypted, "The document could not be read.", nil, err)
	}
	defer packageReader.Close()
	return extractOOXMLFiles(packageReader.File, mediaType, maxDecoded, maxOut)
}

func extractOOXMLFiles(files []*zip.File, mediaType string, maxDecoded, maxOut int64) (string, error) {
	text := ooxmlTextAccumulator{max: maxOut}
	var err error
	switch mediaType {
	case docxMediaType:
		err = extractDOCXText(files, maxDecoded, &text)
	case pptxMediaType:
		err = extractPPTXText(files, maxDecoded, &text)
	case xlsxMediaType:
		err = extractXLSXText(files, maxDecoded, &text)
	default:
		return "", checkOOXMLMediaType(mediaType)
	}
	if errors.Is(err, errOOXMLExtractLimit) {
		return "", NewDocumentError(CodeRequestSizeExceeded, "The extracted document text is too large.", nil, nil)
	}
	if err != nil {
		return "", NewDocumentError(CodeCorruptOrEncrypted, "The document could not be read.", nil, err)
	}
	return text.String(), nil
}

func extractDOCXText(files []*zip.File, maxDecoded int64, text *ooxmlTextAccumulator) error {
	mainPart, err := uniqueOOXMLPart(files, "word/document.xml")
	if err != nil {
		return err
	}
	if mainPart == nil {
		return errors.New("word/document.xml part is missing")
	}

	consumed := int64(0)
	if err := readOOXMLPart(mainPart, &consumed, maxDecoded, func(reader io.Reader) error {
		return extractXMLText(reader, true, text)
	}); err != nil {
		return err
	}
	return readRemainingOOXMLParts(files, map[*zip.File]bool{mainPart: true}, &consumed, maxDecoded)
}

func extractPPTXText(files []*zip.File, maxDecoded int64, text *ooxmlTextAccumulator) error {
	slides, err := findOOXMLParts(files, "ppt/slides/slide")
	if err != nil {
		return err
	}
	consumed := int64(0)
	readParts := make(map[*zip.File]bool, len(slides))
	for index, slide := range slides {
		if index > 0 {
			if err := text.Append("\n\n"); err != nil {
				return err
			}
		}
		readParts[slide.file] = true
		if err := readOOXMLPart(slide.file, &consumed, maxDecoded, func(reader io.Reader) error {
			return extractXMLText(reader, false, text)
		}); err != nil {
			return err
		}
	}
	return readRemainingOOXMLParts(files, readParts, &consumed, maxDecoded)
}

// extractXLSXText uses numeric sheetN.xml order as a best-effort cross-sheet
// order because workbook relationship metadata is not required for extraction.
func extractXLSXText(files []*zip.File, maxDecoded int64, text *ooxmlTextAccumulator) error {
	sharedStringsPart, err := uniqueOOXMLPart(files, "xl/sharedStrings.xml")
	if err != nil {
		return err
	}
	worksheets, err := findOOXMLParts(files, "xl/worksheets/sheet")
	if err != nil {
		return err
	}
	if len(worksheets) == 0 {
		return errors.New("worksheet parts are missing")
	}

	consumed := int64(0)
	shared := []string(nil)
	readParts := make(map[*zip.File]bool, len(worksheets)+1)
	if sharedStringsPart != nil {
		if err := readOOXMLPart(sharedStringsPart, &consumed, maxDecoded, func(reader io.Reader) error {
			var parseErr error
			shared, parseErr = extractSharedStrings(reader)
			return parseErr
		}); err != nil {
			return err
		}
		readParts[sharedStringsPart] = true
	}
	for index, worksheet := range worksheets {
		if index > 0 {
			if err := text.Append("\n\n"); err != nil {
				return err
			}
		}
		readParts[worksheet.file] = true
		if err := readOOXMLPart(worksheet.file, &consumed, maxDecoded, func(reader io.Reader) error {
			return extractWorksheetText(reader, shared, text)
		}); err != nil {
			return err
		}
	}
	return readRemainingOOXMLParts(files, readParts, &consumed, maxDecoded)
}

// uniqueOOXMLPart rejects duplicate package names instead of letting archive order pick the document we read.
func uniqueOOXMLPart(files []*zip.File, name string) (*zip.File, error) {
	var part *zip.File
	for _, file := range files {
		if file.FileInfo().IsDir() || file.Name != name {
			continue
		}
		if part != nil {
			return nil, errors.New("duplicate OOXML part")
		}
		part = file
	}
	return part, nil
}

// ooxmlPart keeps a numbered package file together so callers can process parts in document order.
type ooxmlPart struct {
	file   *zip.File
	number int
}

// findOOXMLParts rejects duplicate numbered files before sorting their numeric suffixes.
func findOOXMLParts(files []*zip.File, prefix string) ([]ooxmlPart, error) {
	parts := make([]ooxmlPart, 0)
	seen := make(map[int]bool)
	for _, file := range files {
		if file.FileInfo().IsDir() {
			continue
		}
		number, ok := ooxmlPartNumber(file.Name, prefix)
		if !ok {
			continue
		}
		if seen[number] {
			return nil, errors.New("duplicate OOXML numbered part")
		}
		seen[number] = true
		parts = append(parts, ooxmlPart{file: file, number: number})
	}
	sort.Slice(parts, func(left, right int) bool {
		return parts[left].number < parts[right].number
	})
	return parts, nil
}

func ooxmlPartNumber(name, prefix string) (int, bool) {
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".xml") {
		return 0, false
	}
	number, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".xml"))
	return number, err == nil && number > 0
}

// readRemainingOOXMLParts drains skipped entries so compressed payloads can't hide outside text-bearing parts.
func readRemainingOOXMLParts(files []*zip.File, readParts map[*zip.File]bool, consumed *int64, maxDecoded int64) error {
	for _, file := range files {
		if file.FileInfo().IsDir() || readParts[file] {
			continue
		}
		if err := readOOXMLPart(file, consumed, maxDecoded, func(reader io.Reader) error {
			_, err := io.Copy(io.Discard, reader)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// readOOXMLPart gives each parser the remaining shared decode budget and accounts for every byte it reads.
func readOOXMLPart(file *zip.File, consumed *int64, maxDecoded int64, handle func(io.Reader) error) error {
	reader, err := file.Open()
	if err != nil {
		return err
	}

	remaining := maxDecoded - *consumed
	if remaining < 0 {
		closeErr := reader.Close()
		if closeErr != nil {
			return closeErr
		}
		return errOOXMLDecodedLimit
	}
	limited := io.LimitReader(reader, remaining+1)
	counting := &ooxmlCountingReader{reader: limited}
	handleErr := handle(counting)
	*consumed += counting.read
	closeErr := reader.Close()
	if *consumed > maxDecoded {
		return errOOXMLDecodedLimit
	}
	if handleErr != nil {
		return handleErr
	}
	return closeErr
}

type ooxmlCountingReader struct {
	reader io.Reader
	read   int64
}

// Read reads from the OOXML part and tracks decoded bytes for the package-wide limit.
func (reader *ooxmlCountingReader) Read(buffer []byte) (int, error) {
	n, err := reader.reader.Read(buffer)
	reader.read += int64(n)
	return n, err
}

// ooxmlTextAccumulator rejects output before strings.Builder can grow past the extraction budget.
type ooxmlTextAccumulator struct {
	builder strings.Builder
	length  int64
	max     int64
}

// Append adds value unless it would exceed the extraction limit.
func (text *ooxmlTextAccumulator) Append(value string) error {
	if int64(len(value)) > text.max-text.length {
		return errOOXMLExtractLimit
	}
	text.builder.WriteString(value)
	text.length += int64(len(value))
	return nil
}

// String returns the accumulated extracted text.
func (text *ooxmlTextAccumulator) String() string {
	return text.builder.String()
}

// extractXMLText admits only OOXML text nodes and adds paragraph breaks where the format needs them.
func extractXMLText(reader io.Reader, paragraphBreaks bool, text *ooxmlTextAccumulator) error {
	decoder := xml.NewDecoder(reader)
	textDepth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local == "t" {
				textDepth++
			}
		case xml.EndElement:
			if value.Name.Local == "t" && textDepth > 0 {
				textDepth--
			}
			if paragraphBreaks && value.Name.Local == "p" {
				if err := text.Append("\n"); err != nil {
					return err
				}
			}
		case xml.CharData:
			if textDepth > 0 {
				if err := text.Append(string(value)); err != nil {
					return err
				}
			}
		}
	}
}

// extractSharedStrings validates the string-table state machine instead of quietly accepting malformed spreadsheets.
func extractSharedStrings(reader io.Reader) ([]string, error) {
	decoder := xml.NewDecoder(reader)
	shared := make([]string, 0)
	var (
		itemDepth int
		textDepth int
		item      strings.Builder
	)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if itemDepth != 0 {
				return nil, errors.New("unterminated shared string")
			}
			return shared, nil
		}
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			switch value.Name.Local {
			case "si":
				if itemDepth != 0 {
					return nil, errors.New("nested shared string")
				}
				itemDepth = 1
				item.Reset()
			case "t":
				if itemDepth > 0 {
					textDepth++
				}
			}
		case xml.EndElement:
			switch value.Name.Local {
			case "t":
				if textDepth > 0 {
					textDepth--
				}
			case "si":
				if itemDepth == 0 {
					return nil, errors.New("unexpected shared string close")
				}
				itemDepth = 0
				shared = append(shared, item.String())
			}
		case xml.CharData:
			if itemDepth > 0 && textDepth > 0 {
				item.Write([]byte(value))
			}
		}
	}
}

// extractWorksheetText preserves sparse-cell positions while resolving shared strings and rejecting invalid nesting.
func extractWorksheetText(reader io.Reader, shared []string, text *ooxmlTextAccumulator) error {
	decoder := xml.NewDecoder(reader)
	var (
		inRow         bool
		inCell        bool
		hasRow        bool
		hasCell       bool
		currentColumn int
		cellType      string
		inValue       bool
		inlineDepth   int
		sharedIndex   strings.Builder
	)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if inRow || inCell {
				return errors.New("unterminated worksheet cell or row")
			}
			return nil
		}
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case xml.StartElement:
			switch value.Name.Local {
			case "row":
				if inRow {
					return errors.New("nested worksheet row")
				}
				if hasRow {
					if err := text.Append("\n"); err != nil {
						return err
					}
				}
				hasRow = true
				inRow = true
				hasCell = false
				currentColumn = -1
			case "c":
				if !inRow || inCell {
					return errors.New("worksheet cell outside row")
				}
				targetColumn := currentColumn + 1
				if reference := attributeValue(value, "r"); reference != "" {
					var valid bool
					targetColumn, valid = worksheetColumnIndex(reference)
					if !valid {
						return errors.New("worksheet cell reference is invalid")
					}
				}
				if targetColumn <= currentColumn {
					return errors.New("worksheet cells are out of order")
				}
				tabCount := targetColumn - currentColumn
				if !hasCell {
					tabCount--
				}
				for range tabCount {
					if err := text.Append("\t"); err != nil {
						return err
					}
				}
				currentColumn = targetColumn
				hasCell = true
				inCell = true
				cellType = attributeValue(value, "t")
				sharedIndex.Reset()
			case "v":
				if inCell {
					inValue = true
				}
			case "t":
				if cellType == "inlineStr" {
					inlineDepth++
				}
			}
		case xml.EndElement:
			switch value.Name.Local {
			case "v":
				inValue = false
			case "t":
				if inlineDepth > 0 {
					inlineDepth--
				}
			case "c":
				if !inCell {
					return errors.New("unexpected worksheet cell close")
				}
				if cellType == "s" {
					index, err := strconv.Atoi(sharedIndex.String())
					if err != nil || index < 0 || index >= len(shared) {
						return errors.New("shared string index is invalid")
					}
					if err := text.Append(shared[index]); err != nil {
						return err
					}
				}
				cellType = ""
				inCell = false
				inValue = false
				inlineDepth = 0
			case "row":
				if !inRow {
					return errors.New("unexpected worksheet row close")
				}
				inRow = false
			}
		case xml.CharData:
			if inValue {
				if cellType == "s" {
					if sharedIndex.Len()+len(value) > 20 {
						return errors.New("shared string index is invalid")
					}
					sharedIndex.Write([]byte(value))
				} else if err := text.Append(string(value)); err != nil {
					return err
				}
			} else if inlineDepth > 0 {
				if err := text.Append(string(value)); err != nil {
					return err
				}
			}
		}
	}
}

func worksheetColumnIndex(reference string) (int, bool) {
	column := 0
	position := 0
	for position < len(reference) {
		character := reference[position]
		if character < 'A' || character > 'Z' {
			break
		}
		column = column*26 + int(character-'A'+1)
		position++
	}
	if position == 0 || position == len(reference) {
		return 0, false
	}
	for ; position < len(reference); position++ {
		if reference[position] < '0' || reference[position] > '9' {
			return 0, false
		}
	}
	return column - 1, true
}

func attributeValue(element xml.StartElement, name string) string {
	for _, attribute := range element.Attr {
		if attribute.Name.Local == name {
			return attribute.Value
		}
	}
	return ""
}
