// Package bedrock implements the Amazon Bedrock Converse protocol.
package bedrock

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type credentials struct{ AccessKey, SecretKey, SessionToken string }

func loadCredentials() (credentials, error) {
	c := credentials{os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SESSION_TOKEN")}
	if c.AccessKey != "" || c.SecretKey != "" {
		if c.AccessKey == "" || c.SecretKey == "" {
			return c, fmt.Errorf("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must both be set")
		}
		return c, nil
	}
	path := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return c, err
		}
		path = filepath.Join(home, ".aws", "credentials")
	}
	profile := os.Getenv("AWS_PROFILE")
	if profile == "" {
		profile = "default"
	}
	file, err := os.Open(path)
	if err != nil {
		return c, fmt.Errorf("AWS credentials: %w", err)
	}
	defer file.Close()
	section := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section != profile || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "aws_access_key_id":
			c.AccessKey = v
		case "aws_secret_access_key":
			c.SecretKey = v
		case "aws_session_token":
			c.SessionToken = v
		}
	}
	if err := scanner.Err(); err != nil {
		return c, err
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return c, fmt.Errorf("AWS profile %q has no access key and secret key", profile)
	}
	return c, nil
}

func awsEncode(text string, slash bool) string {
	const digits = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(text) {
		c := text[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.~", rune(c)) || !slash && c == '/' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(digits[c>>4])
			b.WriteByte(digits[c&15])
		}
	}
	return b.String()
}
func digest(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }
func mac(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sign(req *http.Request, body []byte, c credentials, region, service string, now time.Time) {
	stamp := now.UTC().Format("20060102T150405Z")
	date := now.UTC().Format("20060102")
	req.Header.Set("X-Amz-Date", stamp)
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	headers := map[string]string{"host": req.URL.Host}
	if req.Host != "" {
		headers["host"] = req.Host
	}
	for k, values := range req.Header {
		name := strings.ToLower(k)
		if name == "content-type" || strings.HasPrefix(name, "x-amz-") {
			normalized := make([]string, len(values))
			for i, v := range values {
				normalized[i] = strings.Join(strings.Fields(v), " ")
			}
			headers[name] = strings.Join(normalized, ",")
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, k := range names {
		canonicalHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	queries := []string{}
	for k, values := range req.URL.Query() {
		for _, v := range values {
			queries = append(queries, awsEncode(k, true)+"="+awsEncode(v, true))
		}
	}
	sort.Strings(queries)
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	// Bedrock follows the non-S3 SigV4 rules: encode the escaped path again.
	canonical := strings.Join([]string{req.Method, awsEncode(path, false), strings.Join(queries, "&"), canonicalHeaders.String(), strings.Join(names, ";"), digest(body)}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + digest([]byte(canonical))
	key := mac(mac(mac(mac([]byte("AWS4"+c.SecretKey), date), region), service), "aws4_request")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKey+"/"+scope+", SignedHeaders="+strings.Join(names, ";")+", Signature="+hex.EncodeToString(mac(key, toSign)))
}
