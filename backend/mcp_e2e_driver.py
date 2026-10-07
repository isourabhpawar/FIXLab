#!/usr/bin/env python3
# MCP stdio driver for e2e_phase2_2.sh.
# Usage: mcp_e2e_driver.py <steps.json>
# steps.json: [{"tool": "<name>", "args": {...}}, ...]
# Prints a JSON array of per-step results to stdout:
#   [{"tool":..., "isError":bool, "text": "...", "structured": ...}, ...]
# Each run does its own initialize handshake (cheap); the mcp-server
# process is stateless so this is safe.
import json
import os
import subprocess
import sys


class MCP:
    def __init__(self):
        env = dict(os.environ)
        self.p = subprocess.Popen(
            ["./bin/mcp-server"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
            bufsize=1,
            env=env,
        )
        self._next_id = 0
        self.tools = self._initialize()

    def _rpc(self, method, params=None):
        self._next_id += 1
        rid = self._next_id
        msg = {"jsonrpc": "2.0", "id": rid, "method": method}
        if params is not None:
            msg["params"] = params
        self.p.stdin.write(json.dumps(msg) + "\n")
        self.p.stdin.flush()
        while True:
            line = self.p.stdout.readline()
            if not line:
                raise RuntimeError("mcp-server closed stdout")
            d = json.loads(line)
            if d.get("id") == rid:
                if "error" in d:
                    raise RuntimeError(f"rpc error: {d['error']}")
                return d.get("result")

    def _initialize(self):
        res = self._rpc("initialize", {
            "protocolVersion": "2025-11-25",
            "capabilities": {},
            "clientInfo": {"name": "e2e", "version": "0"},
        })
        self.p.stdin.write(json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}) + "\n")
        self.p.stdin.flush()
        tools = self._rpc("tools/list", {})["tools"]
        return sorted(t["name"] for t in tools)

    def call(self, name, args):
        res = self._rpc("tools/call", {"name": name, "arguments": args})
        text = ""
        for c in res.get("content", []):
            if c.get("type") == "text":
                text += c.get("text", "")
        return {
            "tool": name,
            "isError": bool(res.get("isError", False)),
            "text": text,
            "structured": res.get("structuredContent"),
        }

    def close(self):
        try:
            self.p.stdin.close()
        except Exception:
            pass
        self.p.wait(timeout=5)


def main():
    steps = json.load(open(sys.argv[1]))
    m = MCP()
    out = [{"tool": "__handshake__", "isError": False, "tools": m.tools,
            "text": "", "structured": None}]
    for s in steps:
        out.append(m.call(s["tool"], s.get("args", {})))
    m.close()
    json.dump(out, sys.stdout, indent=1)


main()
