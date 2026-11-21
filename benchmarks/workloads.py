"""Self-contained Python benchmark workloads (stdlib only, Python 3.8+ compatible).

Each workload prints a single JSON line prefixed with 'BENCH_RESULT:' so stdout noise
from wrappers/CLIs can be cleanly separated from benchmark output.
"""

import json
import os
import platform
import socket
import sys
import time


# Compact, self-contained one-liner/script bodies so identical code can be executed
# either locally via `python3 -c` or inside AX via `python3 -c` / stdin / file.

WORKLOAD_SNIPPETS = {
    "env": (
        "import json,os,platform,socket,sys;"
        "info={"
        "'python_version':sys.version,"
        "'executable':sys.executable,"
        "'platform':platform.platform(),"
        "'system':platform.system(),"
        "'machine':platform.machine(),"
        "'hostname':socket.gethostname(),"
        "'cpu_count':os.cpu_count(),"
        "'pid':os.getpid(),"
        "'cwd':os.getcwd()"
        "};"
        "os_rel='';\n"
        "try:\n"
        " os_rel=open('/etc/os-release').read().strip().splitlines()[0]\n"
        "except Exception:\n"
        " pass\n"
        "info['os_release']=os_rel;"
        "print('BENCH_RESULT:'+json.dumps(info))"
    ),
    "noop": (
        "import json,time;"
        "t0=time.perf_counter();"
        "val=1;"
        "t1=time.perf_counter();"
        "print('BENCH_RESULT:'+json.dumps({'workload':'noop','in_python_ms':round((t1-t0)*1000.0,6),'checksum':val}))"
    ),
    "cpu_sieve": (
        "import json,time;"
        "t0=time.perf_counter();"
        "n=1000000;"
        "sieve=bytearray(b'\\x01')*(n+1);"
        "sieve[0:2]=b'\\x00\\x00';"
        "limit=int(n**0.5)+1;"
        "\nfor p in range(2,limit):\n"
        " if sieve[p]:\n"
        "  sieve[p*p:n+1:p]=b'\\x00'*(((n-p*p)//p)+1)\n"
        "primes=[i for i,v in enumerate(sieve) if v];"
        "count=len(primes);"
        "total=sum(primes);"
        "t1=time.perf_counter();"
        "print('BENCH_RESULT:'+json.dumps({'workload':'cpu_sieve','n':n,'in_python_ms':round((t1-t0)*1000.0,3),'prime_count':count,'checksum':total}))"
    ),
    "cpu_loop": (
        "import json,math,time;"
        "t0=time.perf_counter();"
        "acc=0.0;\n"
        "for i in range(1,1500001):\n"
        " acc+=math.sin(i)*math.cos(i)+1.0/(i+1.0)\n"
        "t1=time.perf_counter();"
        "print('BENCH_RESULT:'+json.dumps({'workload':'cpu_loop','iters':1500000,'in_python_ms':round((t1-t0)*1000.0,3),'checksum':round(acc,6)}))"
    ),
    "mem_json": (
        "import json,time;"
        "t0=time.perf_counter();"
        "data=[{'id':i,'group':f'g{i%64}','name':f'item_{i:06d}','values':[i,i*2,(i*3)%997,round(i*0.125,3)],'meta':{'active':(i%2==0),'tag':f't{i%16}'}} for i in range(25000)];"
        "raw=json.dumps(data);"
        "parsed=json.loads(raw);"
        "totals={};\n"
        "for row in parsed:\n"
        " g=row['group'];totals[g]=totals.get(g,0)+row['values'][2]\n"
        "checksum=sum(totals.values());"
        "t1=time.perf_counter();"
        "print('BENCH_RESULT:'+json.dumps({'workload':'mem_json','records':len(parsed),'json_bytes':len(raw),'in_python_ms':round((t1-t0)*1000.0,3),'checksum':checksum}))"
    ),
}

if __name__ == "__main__":
    name = sys.argv[1] if len(sys.argv) > 1 else "env"
    if name not in WORKLOAD_SNIPPETS:
        raise SystemExit(f"Unknown workload: {name}")
    exec(WORKLOAD_SNIPPETS[name], {})
