package cloudinit

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"strings"
)

// boundary is fixed so the user-data is reproducible; assemble refuses content
// that contains it.
const boundary = "KARPENTER-USER-DATA"

type part struct {
	contentType string
	filename    string
	body        string
}

// assemble wraps the parts in the multipart/mixed document cloud-init expects.
// ASCII bodies go through verbatim so the user-data stays readable; anything
// else is base64 encoded, which cloud-init decodes.
func assemble(parts []part) (string, error) {
	var out bytes.Buffer
	fmt.Fprintf(&out,
		"Content-Type: multipart/mixed; boundary=\"%s\"\r\nMIME-Version: 1.0\r\n\r\n",
		boundary,
	)
	writer := multipart.NewWriter(&out)
	if err := writer.SetBoundary(boundary); err != nil {
		return "", err
	}
	for _, p := range parts {
		if strings.Contains(p.body, boundary) {
			return "", fmt.Errorf("the %s part contains the MIME boundary %q", p.filename, boundary)
		}
		body, encoding := p.body, "7bit"
		if !isASCII(body) {
			body, encoding = wrap(base64.StdEncoding.EncodeToString([]byte(body))), "base64"
		}
		header := textproto.MIMEHeader{}
		header.Set("Content-Type", p.contentType+`; charset="utf-8"`)
		header.Set("MIME-Version", "1.0")
		header.Set("Content-Transfer-Encoding", encoding)
		header.Set("Content-Disposition", `attachment; filename="`+p.filename+`"`)
		w, err := writer.CreatePart(header)
		if err != nil {
			return "", err
		}
		if _, err := io.WriteString(w, body); err != nil {
			return "", err
		}
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	return out.String(), nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			return false
		}
	}
	return true
}

// wrap folds base64 text at the 76 columns RFC 2045 allows.
func wrap(encoded string) string {
	const width = 76
	var out strings.Builder
	for len(encoded) > width {
		out.WriteString(encoded[:width])
		out.WriteString("\r\n")
		encoded = encoded[width:]
	}
	out.WriteString(encoded)
	out.WriteString("\r\n")
	return out.String()
}
