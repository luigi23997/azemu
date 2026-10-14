#!/usr/bin/env node
"use strict";

const fs = require("fs");
const path = require("path");
const os = require("os");
const net = require("net");
const cp = require("child_process");

const isWin = process.platform === "win32";
const winDrive = isWin ? process.cwd().slice(0, 2) : "";

// Go's GOOS=js uses Unix filepath semantics: strip Windows drive prefixes (C:\foo -> /foo)
// and convert backslashes to slashes so filepath.IsAbs and filepath.Join work cleanly.
function toPosix(p) {
  if (typeof p !== "string") return p;
  return p.replace(/^[A-Za-z]:/, "").replace(/\\/g, "/");
}

function toHostPath(p) {
  if (!isWin || typeof p !== "string" || p === "") return p;
  if (/^[A-Za-z]:/.test(p)) return p.replace(/\//g, "\\");
  if (p.startsWith("/")) return winDrive + p.replace(/\//g, "\\");
  return p.replace(/\//g, "\\");
}

// Node on Windows lacks O_DIRECTORY; set it to 0 so Go's syscall.Open succeeds on os.ReadDir.
if (fs.constants.O_DIRECTORY === undefined) {
  try {
    Object.defineProperty(fs.constants, "O_DIRECTORY", { value: 0, enumerable: true });
  } catch {
    fs.constants = Object.assign({}, fs.constants, { O_DIRECTORY: 0 });
  }
}

const origCwd = process.cwd.bind(process);
process.cwd = () => toPosix(origCwd());
const origResolve = path.resolve.bind(path);
path.resolve = (...args) => toPosix(origResolve(...args));

function findGoRoot() {
  if (process.env.GOROOT) return process.env.GOROOT;
  try {
    return cp.execFileSync("go", ["env", "GOROOT"], { encoding: "utf8" }).trim();
  } catch {
    const fallback = path.join(os.homedir(), "go-sdk");
    if (fs.existsSync(fallback)) return fallback;
    throw new Error("cannot locate GOROOT (set GOROOT or put `go` on PATH)");
  }
}

const wasmExecCandidates = [
  path.join(__dirname, "wasm_exec.js"),
  (() => {
    try {
      const gr = findGoRoot();
      const p1 = path.join(gr, "lib", "wasm", "wasm_exec.js");
      if (fs.existsSync(p1)) return p1;
      return path.join(gr, "misc", "wasm", "wasm_exec.js");
    } catch {
      return "";
    }
  })(),
].filter(Boolean);

const wasmExecPath = wasmExecCandidates.find((p) => fs.existsSync(p));
if (!wasmExecPath) {
  console.error("azemu: cannot find Go's wasm_exec.js");
  process.exit(1);
}

// Resolve a command name against PATH + PATHEXT on Windows so .cmd/.bat/.exe are found without shell:true warnings.
function lookPath(cmd, envObj) {
  if (!isWin) return cmd;
  const pathKey = Object.keys(envObj).find((k) => k.toUpperCase() === "PATH") || "PATH";
  const pathStr = envObj[pathKey] || process.env.PATH || process.env.Path || "";
  const dirs = pathStr.split(path.delimiter).filter(Boolean);
  const pathext = (envObj.PATHEXT || process.env.PATHEXT || ".COM;.EXE;.BAT;.CMD").split(";");
  const hasSep = cmd.includes("/") || cmd.includes("\\") || /^[A-Za-z]:/.test(cmd);
  const candidates = hasSep ? [toHostPath(cmd)] : dirs.map((d) => path.join(toHostPath(d), cmd));
  const exts = hasSep ? ["", ...pathext] : pathext;

  for (const base of candidates) {
    for (const ext of exts) {
      const p = ext && base.toLowerCase().endsWith(ext.toLowerCase()) ? base : base + ext;
      try {
        if (fs.statSync(p).isFile()) return p;
      } catch {}
    }
  }
  return cmd;
}

function quoteWinArg(a) {
  if (a.length > 0 && !/[ \t\n\v"&|<>^()%!]/.test(a)) return a;
  return '"' + a.replace(/(\\*)"/g, '$1$1\\"').replace(/(\\+)$/, "$1$1") + '"';
}

function parseEnvList(list) {
  if (!list || typeof list.length !== "number" || list.length === 0) {
    return Object.assign({}, process.env);
  }
  const out = {};
  for (let i = 0; i < list.length; i++) {
    const kv = String(list[i]);
    const eq = kv.indexOf("=");
    if (eq > 0) {
      const k = kv.slice(0, eq);
      let v = kv.slice(eq + 1);
      if (isWin && (k.toUpperCase() === "PATH" || k === "VIRTUAL_ENV" || k.endsWith("_BUNDLE") || k.endsWith("_FILE") || k.endsWith("_CERTS") || k === "TF_DATA_DIR")) {
        if (k.toUpperCase() === "PATH") {
          v = v.split(";").map((part) => toHostPath(part)).join(";");
        } else {
          v = toHostPath(v);
        }
      }
      out[k] = v;
    }
  }
  return out;
}

function wrapSocket(sock) {
  const chunks = [];
  let ended = false;
  let errMsg = null;
  let closed = false;
  let handlers = null;

  sock.on("data", (buf) => {
    const u8 = new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength);
    if (handlers) handlers.onData(u8);
    else chunks.push(u8);
  });
  sock.on("end", () => {
    ended = true;
    if (handlers) handlers.onEnd();
  });
  sock.on("error", (err) => {
    errMsg = err ? String(err.code || err.message || err) : "socket error";
    if (handlers) handlers.onError(errMsg);
  });
  sock.on("close", () => {
    closed = true;
    if (handlers) handlers.onClose();
  });

  return {
    attach(onData, onEnd, onError, onClose) {
      handlers = { onData, onEnd, onError, onClose };
      while (chunks.length > 0) {
        onData(chunks.shift());
      }
      if (errMsg) onError(errMsg);
      if (ended) onEnd();
      if (closed) onClose();
    },
    writeBytes(u8) {
      try {
        sock.write(Buffer.from(u8.buffer, u8.byteOffset, u8.byteLength));
        return "";
      } catch (e) {
        return String(e.message || e);
      }
    },
    close() {
      try {
        sock.end();
        sock.destroy();
      } catch {}
    },
    destroy() {
      try {
        sock.destroy();
      } catch {}
    },
  };
}

globalThis.__azemuHost = {
  goos: isWin ? "windows" : process.platform === "darwin" ? "darwin" : "linux",

  spawnProc(opts) {
    try {
      const env = parseEnvList(opts.env);
      const cwd = opts.dir ? toHostPath(opts.dir) : origCwd();
      const shellLine = opts.shell || "";
      const rawArgs = [];
      if (opts.args) {
        for (let i = 0; i < opts.args.length; i++) rawArgs.push(String(opts.args[i]));
      }

      let file;
      let spawnArgs;
      const spawnOpts = {
        cwd,
        env,
        stdio: [opts.inheritStdin ? "inherit" : "ignore", "pipe", "pipe"],
        detached: !isWin,
      };

      if (shellLine) {
        if (isWin) {
          file = process.env.ComSpec || "cmd.exe";
          spawnArgs = ["/d", "/s", "/c", '"' + shellLine + '"'];
          spawnOpts.windowsVerbatimArguments = true;
        } else {
          file = "sh";
          spawnArgs = ["-c", shellLine];
        }
      } else {
        const resolved = lookPath(rawArgs[0], env);
        const rest = rawArgs.slice(1).map((a) => (isWin && a.startsWith("/Users/") ? toHostPath(a) : a));
        if (isWin && /\.(cmd|bat)$/i.test(resolved)) {
          file = process.env.ComSpec || "cmd.exe";
          const cmdStr = [quoteWinArg(resolved), ...rest.map(quoteWinArg)].join(" ");
          spawnArgs = ["/d", "/s", "/c", '"' + cmdStr + '"'];
          spawnOpts.windowsVerbatimArguments = true;
        } else {
          file = resolved;
          spawnArgs = rest;
        }
      }

      const child = cp.spawn(file, spawnArgs, spawnOpts);
      let exited = false;
      const finish = (code, errStr) => {
        if (exited) return;
        exited = true;
        opts.onExit(typeof code === "number" ? code : 1, errStr || "");
      };

      if (child.stdout) {
        child.stdout.on("data", (b) => {
          opts.onStdout(new Uint8Array(b.buffer, b.byteOffset, b.byteLength));
        });
      }
      if (child.stderr) {
        child.stderr.on("data", (b) => {
          opts.onStderr(new Uint8Array(b.buffer, b.byteOffset, b.byteLength));
        });
      }
      child.on("error", (err) => finish(1, String(err.message || err)));
      child.on("close", (code) => finish(code ?? 0, ""));

      return {
        pid: child.pid || 1,
        error: "",
        terminate(force) {
          if (exited || !child.pid) return;
          if (isWin) {
            cp.spawn("taskkill", ["/T", "/F", "/PID", String(child.pid)], { stdio: "ignore" });
          } else {
            try {
              process.kill(-child.pid, force ? "SIGKILL" : "SIGTERM");
            } catch {
              try {
                child.kill(force ? "SIGKILL" : "SIGTERM");
              } catch {}
            }
          }
        },
      };
    } catch (e) {
      return { error: String(e.message || e) };
    }
  },

  listen(host, port, onListening, onError, onConnection) {
    const srv = net.createServer((sock) => {
      const wrapped = wrapSocket(sock);
      onConnection(wrapped, srv.address()?.port || port, sock.remotePort || 0);
    });
    srv.on("error", (err) => onError(String(err.code || err.message || err)));
    srv.listen(port, host, () => {
      const addr = srv.address();
      onListening(addr && typeof addr === "object" ? addr.port : port);
    });
    return {
      close() {
        try {
          srv.close();
        } catch {}
      },
    };
  },

  connect(host, port, onConnect, onError) {
    const sock = net.connect({ host, port });
    const wrapped = wrapSocket(sock);
    let connected = false;
    sock.once("connect", () => {
      connected = true;
      onConnect(wrapped, sock.localPort || 0);
    });
    sock.once("error", (err) => {
      if (!connected) {
        onError(String(err.code || err.message || err));
      }
    });
    return wrapped;
  },
};

globalThis.require = require;
globalThis.fs = fs;
globalThis.path = path;
globalThis.TextEncoder = require("util").TextEncoder;
globalThis.TextDecoder = require("util").TextDecoder;
globalThis.performance ??= require("perf_hooks").performance;
globalThis.crypto ??= require("crypto");

require(wasmExecPath);

// Support two invocation modes:
// 1. As `go test -exec "node azemu-wasm.js"`: argv[2] is a compiled .test wasm binary
// 2. As `azemu <subcommand> ...`: load azemu.wasm next to this script
let wasmFile = path.join(__dirname, "azemu.wasm");
let wasmArgs = ["azemu", ...process.argv.slice(2).map(toPosix)];
if (process.argv[2] && (process.argv[2].endsWith(".test") || process.argv[2].endsWith(".wasm") || process.argv[2].endsWith(".exe")) && fs.existsSync(process.argv[2])) {
  wasmFile = process.argv[2];
  wasmArgs = process.argv.slice(2).map(toPosix);
}

const go = new Go();
go.argv = wasmArgs;
const env = Object.assign({ TMPDIR: toPosix(os.tmpdir()), HOME: toPosix(os.homedir()) }, process.env);
for (const k of ["HOME", "USERPROFILE", "TEMP", "TMP", "LOCALAPPDATA", "APPDATA"]) {
  if (env[k]) env[k] = toPosix(env[k]);
}
go.env = env;
go.exit = process.exit;

// Keep the Node event loop alive while Go is running (Go goroutines waiting on channels/timers).
const keepAlive = setInterval(() => {}, 60_000);
keepAlive.unref?.();

WebAssembly.instantiate(fs.readFileSync(wasmFile), go.importObject)
  .then((result) => go.run(result.instance))
  .catch((err) => {
    console.error(err);
    process.exit(1);
  });
