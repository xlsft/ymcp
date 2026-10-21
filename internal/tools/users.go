package tools

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xlsft/youtrack-mcp/internal/youtrack"
)

const userFields = "id,login,fullName,email,banned,online,guest"

type getCurrentUserIn struct{}

func (t *toolset) registerUsers(s *mcp.Server) {
	add(s, &mcp.Tool{
		Name:        "get_current_user",
		Description: "Returns the YouTrack user the token belongs to.",
		Annotations: readOnly("Get current user"),
	}, func(ctx context.Context, _ getCurrentUserIn) (any, error) {
		var raw json.RawMessage
		err := t.yt.Get(ctx, "users/me", youtrack.Fields(userFields), &raw)
		return raw, err
	})
}
