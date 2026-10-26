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
	maxUploadTotal   = 200 << 20
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
	IssueID   string `json:"issueId,omitempty" jsonschema:"Issue ID, e.g. DEMO-123 (or pass articleId)"`
	ArticleID string `json:"articleId,omitempty" jsonschema:"Knowledge base article ID, e.g. DEMO-A-4 (instead of issueId)"`
}

type getAttachmentIn struct {
	IssueID    string `json:"issueId,omitempty" jsonschema:"Issue ID, e.g. DEMO-123 (or pass articleId)"`
	ArticleID  string `json:"articleId,omitempty" jsonschema:"Knowledge base article ID, e.g. DEMO-A-4 (instead of issueId)"`
	Attachment string `json:"attachment" jsonschema:"Attachment ID (e.g. 12-169) or file name"`
	SaveTo     string `json:"saveTo,omitempty" jsonschema:"Local file or directory to save the attachment to instead of returning it"`
}

type addAttachmentIn struct {
	IssueID     string   `json:"issueId,omitempty" jsonschema:"Issue ID, e.g. DEMO-123 (or pass articleId)"`
	ArticleID   string   `json:"articleId,omitempty" jsonschema:"Knowledge base article ID, e.g. DEMO-A-4 (instead of issueId)"`
	FilePath    string   `json:"filePath,omitempty" jsonschema:"Local file to attach"`
	FilePaths   []string `json:"filePaths,omitempty" jsonschema:"Several local files to attach in one request, e.g. all images of an article"`
	FileName    string   `json:"fileName,omitempty" jsonschema:"Name for a single attachment; required with textContent, defaults to the file's name"`
	TextContent string   `json:"textContent,omitempty" jsonschema:"Attach this text instead of a local file, e.g. a log excerpt"`
}

type deleteAttachmentIn struct {
	IssueID    string `json:"issueId,omitempty" jsonschema:"Issue ID, e.g. DEMO-123 (or pass articleId)"`
	ArticleID  string `json:"articleId,omitempty" jsonschema:"Knowledge base article ID, e.g. DEMO-A-4 (instead of issueId)"`
	Attachment string `json:"attachment" jsonschema:"Attachment ID (e.g. 12-169) or file name"`
}

func (t *toolset) registerAttachments(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name:        "get_issue_attachments",
		Description: "Lists the attachments of an issue or a knowledge base article: ID, file name, size, type, author and date.",
		Annotations: readOnly("Get attachments"),
	}, t.getIssueAttachments)

	add(s, &mcp.Tool{
		Name: "get_attachment",
		Description: "Reads an attachment of an issue or article. Text files are returned as text and images as images; " +
			"other files, or anything too large to inline, are saved to saveTo on this machine.",
		Annotations: readOnly("Get attachment"),
	}, t.getAttachment)

	add(s, &mcp.Tool{
		Name: "add_attachment",
		Description: "Attaches local files (filePath, or several with filePaths), or the given text as a file, to an issue " +
			"or a knowledge base article. Article text can then reference the images by file name. Returns the attachment IDs.",
		Annotations: changes("Add attachment"),
	}, t.addAttachment)

	add(s, &mcp.Tool{
		Name:        "delete_attachment",
		Description: "Permanently deletes an attachment from an issue or article.",
		Annotations: removes("Delete attachment"),
	}, t.deleteAttachment)
}

// attachmentOwner is the issue or article whose attachments a tool works on.
type attachmentOwner struct {
	key, id string // "issueId" or "articleId", and the ID
}

func ownerOf(issueID, articleID string) (attachmentOwner, error) {
	switch {
	case issueID != "" && articleID != "":
		return attachmentOwner{}, fmt.Errorf("pass either issueId or articleId, not both")
	case articleID != "":
		return attachmentOwner{"articleId", articleID}, nil
	case issueID != "":
		return attachmentOwner{"issueId", issueID}, nil
	}
	return attachmentOwner{}, fmt.Errorf("pass issueId or articleId")
}

// path is the REST collection of the owner's attachments.
func (o attachmentOwner) path() string {
	if o.key == "articleId" {
		return "articles/" + url.PathEscape(o.id) + "/attachments"
	}
	return "issues/" + url.PathEscape(o.id) + "/attachments"
}

func (t *toolset) attachments(ctx context.Context, o attachmentOwner) ([]apiAttachment, error) {
	var list []apiAttachment
	err := t.yt.Get(ctx, o.path(), url.Values{"fields": {attachmentFields}, "$top": {"-1"}}, &list)
	return list, err
}

func (t *toolset) findAttachment(ctx context.Context, o attachmentOwner, ref string) (apiAttachment, error) {
	issueID := o.id
	list, err := t.attachments(ctx, o)
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
	o, err := ownerOf(in.IssueID, in.ArticleID)
	if err != nil {
		return nil, err
	}
	list, err := t.attachments(ctx, o)
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
	return map[string]any{o.key: o.id, "attachments": out}, nil
}

func (t *toolset) getAttachment(ctx context.Context, in getAttachmentIn) (any, error) {
	o, err := ownerOf(in.IssueID, in.ArticleID)
	if err != nil {
		return nil, err
	}
	a, err := t.findAttachment(ctx, o, in.Attachment)
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
		return map[string]any{o.key: o.id, "attachment": a.Name, "savedTo": path, "size": len(data)}, nil
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
	o, err := ownerOf(in.IssueID, in.ArticleID)
	if err != nil {
		return nil, err
	}
	paths := in.FilePaths
	if in.FilePath != "" {
		paths = append([]string{in.FilePath}, paths...)
	}
	var files []youtrack.File
	switch {
	case len(paths) > 0 && in.TextContent != "":
		return nil, fmt.Errorf("pass either files or textContent, not both")
	case len(paths) > 0:
		if in.FileName != "" && len(paths) > 1 {
			return nil, fmt.Errorf("fileName only renames a single file")
		}
		var total int64
		for _, p := range paths {
			info, err := os.Stat(p)
			if err != nil {
				return nil, err
			}
			if info.Size() > maxUpload {
				return nil, fmt.Errorf("%s is larger than %d MB", p, maxUpload>>20)
			}
			if total += info.Size(); total > maxUploadTotal {
				return nil, fmt.Errorf("files add up to more than %d MB; attach them in several calls", maxUploadTotal>>20)
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			files = append(files, youtrack.File{Name: filepath.Base(p), Content: data})
		}
	case in.TextContent != "":
		if in.FileName == "" {
			return nil, fmt.Errorf("fileName is required with textContent")
		}
		files = []youtrack.File{{Content: []byte(in.TextContent)}}
	default:
		return nil, fmt.Errorf("pass filePath, filePaths or textContent")
	}
	if in.FileName != "" {
		files[0].Name = in.FileName
	}

	var created []apiAttachment
	if err := t.yt.Upload(ctx, o.path(), url.Values{"fields": {"id,name,size,mimeType"}}, files, &created); err != nil {
		return nil, err
	}
	if len(created) == 0 {
		return nil, fmt.Errorf("YouTrack accepted the upload but returned no attachment")
	}
	type attachment struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		MimeType string `json:"mimeType,omitempty"`
	}
	list := make([]attachment, 0, len(created))
	for _, c := range created {
		list = append(list, attachment{c.ID, c.Name, c.Size, c.MimeType})
	}
	out := map[string]any{o.key: o.id, "attachments": list}
	if len(list) == 1 {
		// A single upload also answers flat, as before multi-file uploads.
		out["id"], out["name"], out["size"], out["mimeType"] = list[0].ID, list[0].Name, list[0].Size, list[0].MimeType
	}
	return out, nil
}

func (t *toolset) deleteAttachment(ctx context.Context, in deleteAttachmentIn) (any, error) {
	o, err := ownerOf(in.IssueID, in.ArticleID)
	if err != nil {
		return nil, err
	}
	a, err := t.findAttachment(ctx, o, in.Attachment)
	if err != nil {
		return nil, err
	}
	if err := t.yt.Delete(ctx, o.path()+"/"+url.PathEscape(a.ID), nil); err != nil {
		return nil, err
	}
	return map[string]any{o.key: o.id, "deleted": a.Name, "id": a.ID}, nil
}
