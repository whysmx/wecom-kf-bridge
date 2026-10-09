#!/usr/bin/env python3
"""Strict Go statement coverage gate: 100*covered > threshold*total."""
from __future__ import annotations
import argparse, collections, pathlib, re, subprocess, sys

LINE = re.compile(r"^(.*):(\d+\.\d+,\d+\.\d+) (\d+) (\d+)$")
def check(path: pathlib.Path, threshold: int = 95, expected: list[str] | None = None) -> tuple[int,int,dict[str,tuple[int,int]]]:
    if not path.is_file(): raise ValueError(f"coverage profile not found: {path}")
    lines=path.read_text(encoding="utf-8").splitlines()
    if not lines or not lines[0].startswith("mode: "): raise ValueError("coverage profile missing mode")
    seen={}; total=covered=0; groups=collections.defaultdict(lambda:[0,0])
    for n,line in enumerate(lines[1:],2):
        if not line.strip(): continue
        m=LINE.match(line)
        if not m: raise ValueError(f"malformed coverage line {n}")
        filename, block, statements, count=m.groups(); statements=int(statements); count=int(count)
        if statements < 0 or count < 0: raise ValueError(f"invalid coverage count line {n}")
        # Go emits zero-statement synthetic blocks for some closing braces.
        # They are not executable and must not affect either denominator.
        if statements == 0:
            continue
        key=(filename,block,statements)
        # A multi-package `go test -coverpkg=./...` profile may repeat an
        # identical block once per test binary. Merge counts by OR/max:
        # each package's test binary observes a different subset, and a block
        # is covered when any binary executed it.
        previous=seen.get(key)
        if previous is not None:
            if count > previous:
                seen[key] = count
                if previous == 0:
                    covered += statements
                    group_name = str(pathlib.PurePosixPath(filename.replace("\\", "/")).parent)
                    groups[group_name][1] += statements
            continue
        seen[key]=count; total+=statements; covered+=statements if count>0 else 0
        group_name = str(pathlib.PurePosixPath(filename.replace("\\", "/")).parent)
        groups[group_name][0]+=statements
        groups[group_name][1]+=statements if count>0 else 0
    if total==0: raise ValueError("coverage profile has zero statements")
    if 100*covered <= threshold*total: raise ValueError(f"overall coverage {100*covered/total:.2f}% is not strictly greater than {threshold}%")
    result={k:(v[0],v[1]) for k,v in groups.items()}
    if expected:
        missing=[pkg for pkg in expected if pkg not in result]
        if missing: raise ValueError("production packages missing from profile: " + ", ".join(missing))
    for pkg,(t,c) in result.items():
        if t==0 or 100*c <= threshold*t: raise ValueError(f"package {pkg} coverage {100*c/t if t else 0:.2f}% is not strictly greater than {threshold}%")
    return covered,total,result

def main()->int:
    p=argparse.ArgumentParser(); p.add_argument("profile",nargs="?",default="coverage.out");p.add_argument("--threshold",type=int,default=95);p.add_argument("--packages",help="file containing go list ./... import paths") ; a=p.parse_args()
    expected=None
    if a.packages:
        try: expected=[x.strip() for x in pathlib.Path(a.packages).read_text(encoding="utf-8").splitlines() if x.strip()]
        except OSError as e: print(f"coverage gate: FAIL: cannot read package list: {e}",file=sys.stderr); return 1
    try: c,t,g=check(pathlib.Path(a.profile),a.threshold,expected)
    except (OSError,ValueError) as e: print(f"coverage gate: FAIL: {e}",file=sys.stderr); return 1
    print(f"coverage gate: PASS: {c}/{t} ({100*c/t:.2f}%) strict > {a.threshold}% ({len(g)} packages)"); return 0
if __name__=="__main__": raise SystemExit(main())
