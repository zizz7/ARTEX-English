package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

type toolUsageRecorder interface {
	InsertToolUsage(*db.ToolUsage) error
}

// meteredTool records one row immediately before a catalog tool is invoked. It
// embeds the resolved tool so schema overrides, defaults and permission behavior
// remain unchanged.
type meteredTool struct {
	actool.CoreTool
	recorder toolUsageRecorder
	toolKey  string
	agentKey string
	ri       agent.RunInfo
}

func meterTool(t actool.CoreTool, recorder toolUsageRecorder, toolKey, agentKey string, ri agent.RunInfo) actool.CoreTool {
	if recorder == nil {
		return t
	}
	return &meteredTool{CoreTool: t, recorder: recorder, toolKey: toolKey, agentKey: agentKey, ri: ri}
}

func (m *meteredTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (result actool.Result, err error) {
	if ierr := m.recorder.InsertToolUsage(&db.ToolUsage{
		ToolKey:       m.toolKey,
		AgentKey:      m.agentKey,
		TaskID:        m.ri.TaskID,
		ExplorationID: m.ri.ExplorationID,
		IntentID:      m.ri.IntentID,
		SessionID:     m.ri.SessionID,
	}); ierr != nil {
		log.Printf("[toolusage] insert %s: %v", m.toolKey, ierr)
	}
	// A tool panic (e.g. a task-scoped tool invoked in a context without an
	// exploration store) must not take down the whole process — norma does not
	// recover tool panics. Convert it into a tool error the agent can handle.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[tool] panic in %s recovered: %v", m.toolKey, r)
			result = actool.Errorf(fmt.Sprintf("tool %s failed with an internal error: %v", m.toolKey, r))
			err = nil
		}
	}()
	return m.CoreTool.Call(ctx, input, tc)
}
