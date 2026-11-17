import sys, json
def call(i, m, p):
    sys.stdout.write(json.dumps({"jsonrpc":"2.0","id":i,"method":m,"params":p})+"\n"); sys.stdout.flush()
    return sys.stdin.readline()
call(1, "handshake", {"version":1})
call(2, "effect", {"op":"model","key":"ask","value":"QW5zd2Vy"})
call(3, "step", {"name":"action"})
