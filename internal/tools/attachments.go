package tools

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/ymcp/internal/youtrack"
)

const (
	attachmentFields = "id,name,size,mimeType,url,created,author(login)"
	maxInlineText    = 256 << 10
	maxInlineImage   = 4 << 20
	maxUpload        = 50 << 20
)

type apiAttachment struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Size     int64    `json:"size"`
	MimeType string   `json:"mimeType"`
	URL      string   `json:"url"`
	Created  millis   `json:"created"`
	Author   *userRef `json:"author"`
}

type getIssueAttachmentsIn struct {
	IssueID string `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
}

type getAttachmentIn struct {
	IssueID    string `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
	Attachment string `json:"attachment" jsonschema:"Attachment ID (e.g. 12-169) or file name"`
	SaveTo     string `json:"saveTo,omitempty" jsonschema:"Local file or directory to save the attachment to instead of returning it"`
}

type addAttachmentIn struct {
	IssueID     string `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
	FilePath    string `json:"filePath,omitempty" jsonschema:"Local file to attach"`
	FileName    string `json:"fileName,omitempty" jsonschema:"Name for the attachment; required with textContent, defaults to the file's name"`
	TextContent string `json:"textContent,omitempty" jsonschema:"Attach this text instead of a local file, e.g. a log excerpt"`
}

type deleteAttachmentIn struct {
	IssueID    string `json:"issueId" jsonschema:"Issue ID, e.g. DEMO-123"`
	Attachment string `json:"attachment" jsonschema:"Attachment ID (e.g. 12-169) or file name"`
}

func (t *toolset) registerAttachments(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name:        "get_issue_attachments",
		Description: "Lists the attachments of an issue: ID, file name, size, type, author and date.",
		Annotations: readOnly("Get issue attachments"),
	}, t.getIssueAttachments)

	add(s, &mcp.Tool{
		Name: "get_attachment",
		Description: "Reads an attachment. Text files are returned as text and images as images; " +
			"other files, or anything too large to inline, are saved to saveTo on this machine.",
		Annotations: readOnly("Get attachment"),
	}, t.getAttachment)

	add(s, &mcp.Tool{
		Name:        "add_attachment",
		Description: "Attaches a local file, or the given text as a file, to an issue. Returns the attachment ID.",
		Annotations: mutating("Add attachment", false),
	}, t.addAttachment)

	add(s, &mcp.Tool{
		Name:        "delete_attachment",
		Description: "Permanently deletes an attachment from an issue.",
		Annotations: mutating("Delete attachment", true),
	}, t.deleteAttachment)
}

func (t *toolset) attachments(ctx context.Context, issueID string) ([]apiAttachment, error) {
	var list []apiAttachment
	path := "issues/" + url.PathEscape(issueID) + "/attachments"
	err := t.yt.Get(ctx, path, url.Values{"fields": {attachmentFields}, "$top": {"-1"}}, &list)
	return list, err
}

func (t *toolset) findAttachment(ctx context.Context, issueID, ref string) (apiAttachment, error) {
	list, err := t.attachments(ctx, issueID)
	if err != nil {
		return apiAttachment{}, err
	}
	for _, a := range list {
		if a.ID == ref || a.Name == ref {
			return a, nil
		}
	}
	names := make([]string, 0, len(list))
	for _, a := range list {
		names = append(names, fmt.Sprintf("%s (%s)", a.Name, a.ID))
	}
	if len(names) == 0 {
		return apiAttachment{}, fmt.Errorf("%s has no attachments", issueID)
	}
	return apiAttachment{}, fmt.Errorf("%s has no attachment %q; attachments: %s", issueID, ref, strings.Join(names, ", "))
}

func (t *toolset) getIssueAttachments(ctx context.Context, in getIssueAttachmentsIn) (any, error) {
	list, err := t.attachments(ctx, in.IssueID)
	if err != nil {
		return nil, err
	}
	type attachment struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		MimeType string `json:"mimeType,omitempty"`
		Created  millis `json:"created"`
		Author   string `json:"author,omitempty"`
	}
	out := make([]attachment, 0, len(list))
	for _, a := range list {
		at := attachment{ID: a.ID, Name: a.Name, Size: a.Size, MimeType: a.MimeType, Created: a.Created}
		if a.Author != nil {
			at.Author = a.Author.Login
		}
		out = append(out, at)
	}
	return map[string]any{"issueId": in.IssueID, "attachments": out}, nil
}

func (t *toolset) getAttachment(ctx context.Context, in getAttachmentIn) (any, error) {
	a, err := t.findAttachment(ctx, in.IssueID, in.Attachment)
	if err != nil {
		return nil, err
	}
	image := strings.HasPrefix(a.MimeType, "image/") && a.MimeType != "image/svg+xml"
	if in.SaveTo == "" && !image && a.Size > maxInlineText {
		return nil, fmt.Errorf("%s is %d bytes, too large to return inline; pass saveTo", a.Name, a.Size)
	}
	if in.SaveTo == "" && image && a.Size > maxInlineImage {
		return nil, fmt.Errorf("image %s is %d bytes, too large to return inline; pass saveTo", a.Name, a.Size)
	}
	data, contentType, err := t.yt.Download(ctx, a.URL)
	if err != nil {
		return nil, err
	}
	if contentType == "" {
		contentType = a.MimeType
	}

	if in.SaveTo != "" {
		path := in.SaveTo
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			path = filepath.Join(path, filepath.Base(a.Name))
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, err
		}
		return map[string]any{"issueId": in.IssueID, "attachment": a.Name, "savedTo": path, "size": len(data)}, nil
	}
	if image {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("%s (%s, %d bytes)", a.Name, a.MimeType, len(data))},
			&mcp.ImageContent{Data: data, MIMEType: strings.Split(contentType, ";")[0]},
		}}, nil
	}
	if !isText(contentType, data) {
		return nil, fmt.Errorf("%s is a binary file (%s); pass saveTo to save it", a.Name, a.MimeType)
	}
	return string(data), nil
}

// isText reports whether a file can be returned as text.
func isText(contentType string, data []byte) bool {
	ct := strings.ToLower(contentType)
	for _, kind := range []string{"text/", "json", "xml", "yaml", "javascript", "csv", "markdown", "x-sh"} {
		if strings.Contains(ct, kind) {
			return utf8.Valid(data)
		}
	}
	return utf8.Valid(data) && !bytes.ContainsRune(data, 0)
}

func (t *toolset) addAttachment(ctx context.Context, in addAttachmentIn) (any, error) {
	var file youtrack.File
	switch {
	case in.FilePath != "" && in.TextContent != "":
		return nil, fmt.Errorf("pass either filePath or textContent, not both")
	case in.FilePath != "":
		info, err := os.Stat(in.FilePath)
		if err != nil {
			return nil, err
		}
		if info.Size() > maxUpload {
			return nil, fmt.Errorf("%s is larger than %d MB", in.FilePath, maxUpload>>20)
		}
		data, err := os.ReadFile(in.FilePath)
		if err != nil {
			return nil, err
		}
		file = youtrack.File{Name: filepath.Base(in.FilePath), Content: data}
	case in.TextContent != "":
		if in.FileName == "" {
			return nil, fmt.Errorf("fileName is required with textContent")
		}
		file = youtrack.File{Content: []byte(in.TextContent)}
	default:
		return nil, fmt.Errorf("pass filePath or textContent")
	}
	if in.FileName != "" {
		file.Name = in.FileName
	}

	var created []apiAttachment
	path := "issues/" + url.PathEscape(in.IssueID) + "/attachments"
	if err := t.yt.Upload(ctx, path, url.Values{"fields": {"id,name,size,mimeType"}}, []youtrack.File{file}, &created); err != nil {
		return nil, err
	}
	if len(created) == 0 {
		return nil, fmt.Errorf("YouTrack accepted the upload but returned no attachment")
	}
	c := created[0]
	return map[string]any{"issueId": in.IssueID, "id": c.ID, "name": c.Name, "size": c.Size, "mimeType": c.MimeType}, nil
}

func (t *toolset) deleteAttachment(ctx context.Context, in deleteAttachmentIn) (any, error) {
	a, err := t.findAttachment(ctx, in.IssueID, in.Attachment)
	if err != nil {
		return nil, err
	}
	if err := t.yt.Delete(ctx, "issues/"+url.PathEscape(in.IssueID)+"/attachments/"+url.PathEscape(a.ID), nil); err != nil {
		return nil, err
	}
	return map[string]any{"issueId": in.IssueID, "deleted": a.Name, "id": a.ID}, nil
}
