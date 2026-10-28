"""Container-only protocol adapter. Docker, not this Python code, is the boundary."""

import json
import sys


def main():
    raw = sys.stdin.buffer.read(256 * 1024 + 1)
    if len(raw) > 256 * 1024:
        return 1
    request = json.loads(raw)
    namespace = {"__name__": "__transformation__"}
    exec(compile(request["script"], "<transformation>", "exec"), namespace)
    result = namespace["transform"](request["input"])
    if type(result) is not dict:
        return 2
    encoded = json.dumps(result, allow_nan=False, separators=(",", ":"))
    if len(encoded.encode("utf-8")) > 64 * 1024:
        return 3
    sys.stdout.write(encoded)
    sys.stdout.flush()
    return 0


if __name__ == "__main__":
    try:
        exit_code = main()
    except BaseException:
        # Do not disclose source, input or arbitrary exception strings.
        exit_code = 1
    raise SystemExit(exit_code)
