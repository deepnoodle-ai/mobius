# deepnoodle-mobius

Python SDK for Mobius agent sessions, loops, and workers.

## Multi-tool agent quickstart

```python
import os
import deepnoodle.mobius as mobius

client = mobius.Client(mobius.ClientOptions(
    api_key=os.environ["MOBIUS_API_KEY"],
    project=os.environ["MOBIUS_PROJECT"],
))

try:
    turn = client.invoke_agent(mobius.InvokeAgentOptions(
        agent_name="launch-scout",
        idempotency_key=inbound_message_id,
        content=[{"type": "text", "text": "Check the name and create a shortlist."}],
        config=mobius.InlineAgentConfig(toolkits=[
            mobius.InlineToolkit(name="naming", actions=["naming.domain.check"]),
            mobius.InlineToolkit(name="shortlists", actions=["shortlists.create"]),
        ]),
    ))
    for update in turn.updates():
        for message in update.transcript.renderable_messages():
            print(mobius.text_of(message))
            for block in message["content"]:
                if block.get("type") == "tool_use":
                    tool = mobius.normalize_tool_use(block)
                    print((tool.resolved_action or {}).get("name", tool.wire_name))
    if turn.error:
        raise turn.error
except mobius.MobiusAPIError as error:
    if error.code == "session_turn_active":
        # Choose explicitly: wait and invoke again, or client.nudge_session(...).
        pass
    else:
        raise
```

Use `messages()` for the lossless protocol view and `renderable_messages()`
for UI. `normalize_tool_use()` preserves the model-facing wire fields and
prefers Mobius' persisted canonical action identity. The
[cross-language helper guide](../docs/sdk-helpers.md) covers nudges,
reconnection, diagnostics, session management, persistence ordering, and the
server-to-browser boundary.
