# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Grade a SWE-bench eval log the way the SWE-bench harness does.

    python swebench-grade.py INSTANCE.json EVAL.log

INSTANCE.json is the dataset record (log_parser, eval_type, fail_to_pass,
pass_to_pass). The log is parsed with the repo's parser into a test -> status
map. As in the harness, each listed test is a success, a failure, or neither
(a status the harness does not know counts for nothing); the instance is
resolved when no FAIL_TO_PASS or PASS_TO_PASS test is a failure. Exit 0 when
resolved, 1 otherwise,
2 when the log shows the run itself was invalid (patch did not apply, tests
errored or timed out). One JSON line with the counts goes to stdout.

Standard library only: it runs inside the task image with whatever Python
the image carries.
"""

import json
import re
import sys

PASSED, FAILED, SKIPPED, ERROR, XFAIL = "PASSED", "FAILED", "SKIPPED", "ERROR", "XFAIL"
STATUSES = (FAILED, PASSED, SKIPPED, ERROR, XFAIL)
PASSING = {PASSED, XFAIL}

START_OUTPUT = ">>>>> Start Test Output"
END_OUTPUT = ">>>>> End Test Output"
BAD_MARKERS = (">>>>> Patch Apply Failed", ">>>>> Reset Failed", ">>>>> Tests Errored", ">>>>> Tests Timed Out")
EXIT_CODE = re.compile(r">>>>> Test Exit Code:\s*(-?\d+)")
# Evidence that a suite ran at all, for logs whose parser records only
# failures; mirrors the harness's SUITE_RAN.
SUITE_RAN = re.compile(
    r"Executed [1-9]\d* of \d+"
    r"|TOTAL: [1-9]\d* (?:SUCCESS|FAILED)"
    r"|[1-9]\d* passing"
    r"|Tests:\s+[1-9]\d*"
    r"|Test Suites:\s+(?:\d+ \w+, )*[1-9]\d* total"
    r"|^# tests [1-9]\d*"
    r"|[1-9]\d* specs?, \d+ failures?"
    r"|': ok$",
    re.M,
)
SKIP_SUMMARY = re.compile(r"^\[\d+\]$")
CONTROL = str.maketrans("", "", "".join(chr(c) for c in range(1, 32)))


def _status_line(line, status_map, rewrite=None):
    """pytest -rA short summary: '<STATUS> <test id>[ - reason]'."""
    if not line.startswith(STATUSES):
        return
    if line.startswith(FAILED):
        line = line.replace(" - ", " ")
    parts = line.split()
    if len(parts) < 2:
        return
    status, name = parts[0], parts[1]
    if status == SKIPPED and SKIP_SUMMARY.match(name):
        return
    if rewrite:
        name = rewrite(name)
    status_map[name] = status


def parse_pytest(log):
    status_map = {}
    for line in log.split("\n"):
        _status_line(line, status_map)
    return status_map


_OPTION = re.compile(r"(.*?)\[(.*)\]")


def parse_pytest_options(log):
    def rewrite(name):
        m = _OPTION.search(name)
        if not m:
            return name
        main, option = m.groups()
        if option.startswith("/") and not option.startswith("//") and "*" not in option:
            option = "/" + option.split("/")[-1]
        return f"{main}[{option}]"

    status_map = {}
    for line in log.split("\n"):
        _status_line(line, status_map, rewrite)
    return status_map


def parse_pytest_v2(log):
    status_map = {}
    for line in log.split("\n"):
        line = re.sub(r"\[(\d+)m", "", line).translate(CONTROL)
        if line.startswith(STATUSES):
            if line.startswith(FAILED):
                line = line.split(" - ", 1)[0]
            parts = line.split()
            if len(parts) >= 2 and not (parts[0] == SKIPPED and SKIP_SUMMARY.match(parts[1])):
                status_map[" ".join(parts[1:])] = parts[0]
        elif line.endswith(STATUSES):
            parts = line.split()
            if len(parts) >= 2:
                status_map[" ".join(parts[:-1])] = parts[-1]
    return status_map


def parse_matplotlib(log):
    log = log.replace("MouseButton.LEFT", "1").replace("MouseButton.RIGHT", "3")
    return parse_pytest(log)


def parse_seaborn(log):
    status_map = {}
    for line in log.split("\n"):
        parts = line.split()
        if len(parts) < 2:
            continue
        if line.startswith(FAILED):
            status_map[parts[1]] = FAILED
        elif f" {PASSED} " in line and parts[1] == PASSED:
            status_map[parts[0]] = PASSED
        elif line.startswith(PASSED):
            status_map[parts[1]] = PASSED
    return status_map


def parse_sympy(log):
    status_map = {}
    for m in re.findall(r"(_*) (.*)\.py:(.*) (_*)", log):
        status_map[f"{m[1]}.py:{m[2]}"] = FAILED
    for line in log.split("\n"):
        line = line.strip()
        if not line.startswith("test_"):
            continue
        name = line.split()[0]
        if line.endswith(" E"):
            status_map[name] = ERROR
        elif line.endswith(" F"):
            status_map[name] = FAILED
        elif line.endswith(" ok"):
            status_map[name] = PASSED
    return status_map


def parse_django(log):
    status_map = {}
    prev_test = None
    for raw in log.split("\n"):
        line = raw.strip()
        if "--version is equivalent to version" in line:
            status_map["--version is equivalent to version"] = PASSED
        if " ... " in line:
            prev_test = line.split(" ... ")[0]
        for suffix in (" ... ok", " ... OK", " ...  OK"):
            if line.endswith(suffix):
                if line.startswith("Applying sites.0002_alter_domain_unique...test_no_migrations"):
                    line = line.split("...", 1)[-1].strip()
                status_map[line.rsplit(suffix, 1)[0]] = PASSED
                break
        if " ... skipped" in line:
            status_map[line.split(" ... skipped")[0]] = SKIPPED
        if line.endswith(" ... FAIL"):
            status_map[line.split(" ... FAIL")[0]] = FAILED
        if line.startswith("FAIL:"):
            status_map[line.split()[1].strip()] = FAILED
        if line.endswith(" ... ERROR"):
            status_map[line.split(" ... ERROR")[0]] = ERROR
        if line.startswith("ERROR:"):
            status_map[line.split()[1].strip()] = ERROR
        if line.startswith("ok") and prev_test is not None:
            status_map[prev_test] = PASSED
    for pattern in (
        r"^(.*?)\s\.\.\.\sTesting\ against\ Django\ installed\ in\ ((?s:.*?))\ silenced\)\.\nok$",
        r"^(.*?)\s\.\.\.\sInternal\ Server\ Error:\ \/(.*)\/\nok$",
        r"^(.*?)\s\.\.\.\sSystem check identified no issues \(0 silenced\)\nok$",
    ):
        for m in re.finditer(pattern, log, re.MULTILINE):
            status_map[m.group(1)] = PASSED
    return status_map


PARSERS = {
    "parse_log_pytest": parse_pytest,
    "parse_log_astroid": parse_pytest,
    "parse_log_flask": parse_pytest,
    "parse_log_marshmallow": parse_pytest,
    "parse_log_pvlib": parse_pytest,
    "parse_log_pyvista": parse_pytest,
    "parse_log_sqlfluff": parse_pytest,
    "parse_log_xarray": parse_pytest,
    "parse_log_pytest_options": parse_pytest_options,
    "parse_log_pydicom": parse_pytest_options,
    "parse_log_requests": parse_pytest_options,
    "parse_log_pylint": parse_pytest_options,
    "parse_log_pytest_v2": parse_pytest_v2,
    "parse_log_astropy": parse_pytest_v2,
    "parse_log_scikit": parse_pytest_v2,
    "parse_log_sphinx": parse_pytest_v2,
    "parse_log_matplotlib": parse_matplotlib,
    "parse_log_seaborn": parse_seaborn,
    "parse_log_sympy": parse_sympy,
    "parse_log_django": parse_django,
}


def resolve_case(case, status_map):
    """Exact match, or a prefix match for ids truncated mid-parameter when the
    candidates agree on the outcome (676 Verified ids are truncated)."""
    if case in status_map:
        return case
    if case.count("[") > case.count("]"):
        matches = [k for k in status_map if k.startswith(case)]
        if matches and len({status_map[k] in PASSING for k in matches}) == 1:
            return matches[0]
    return None


def passed(case, status_map):
    key = resolve_case(case, status_map)
    return key is not None and status_map[key] in PASSING


def maintained(case, status_map):
    key = resolve_case(case, status_map)
    return passed(case, status_map) or (key is not None and status_map[key] == SKIPPED)


def failed(case, status_map):
    """Missing, FAILED, ERROR or SKIPPED, as the harness's test_failed."""
    key = resolve_case(case, status_map)
    return key is None or status_map[key] in (FAILED, ERROR, SKIPPED)


def status_map_from_log(parser, content):
    """The harness's validity rules, then the parser over the marked region."""
    if any(marker in content for marker in BAD_MARKERS):
        return None
    # The harness slices from the first Start marker to the End marker after
    # it, thus a stray End before a correct pair is ignored here too. Two
    # differences remain, both stricter: a log whose only End comes before
    # Start is invalid (the harness grades the whole log), and a second Start
    # does not end the region (the harness stops there).
    start = content.find(START_OUTPUT)
    end = content.find(END_OUTPUT, start + len(START_OUTPUT)) if start >= 0 else -1
    if start < 0 or end < 0:
        return None
    sliced = content[start + len(START_OUTPUT):end]
    status_map = parser(sliced) or parser(content)
    if not status_map and not SUITE_RAN.search(content):
        return None
    m = EXIT_CODE.search(content)
    exit_code = int(m.group(1)) if m else None
    if exit_code not in (None, 0) and status_map and not any(s in (FAILED, ERROR) for s in status_map.values()):
        return None
    return status_map


def grade(instance, content):
    parser = PARSERS.get(instance.get("log_parser", ""))
    if parser is None:
        raise SystemExit(f"unknown log_parser {instance.get('log_parser')!r}")
    status_map = status_map_from_log(parser, content)
    if status_map is None:
        return {"valid": False, "resolved": False}
    fail_only = instance.get("eval_type", "pass_and_fail").lower() == "fail_only"
    f2p = instance.get("fail_to_pass", [])
    p2p = instance.get("pass_to_pass", [])

    # As in the harness, every test lands in a success list, a failure list,
    # or neither (an unknown status counts for nothing); the instance is
    # resolved when both failure lists are empty.
    def split(tests, ok, bad):
        success = [t for t in tests if ok(t)]
        failure = [t for t in tests if not ok(t) and bad(t)]
        return success, failure

    if fail_only:
        bad = lambda t: status_map.get(t) == FAILED  # noqa: E731
        f2p_ok, f2p_bad = split(f2p, lambda t: not bad(t), bad)
        p2p_ok, p2p_bad = split(p2p, lambda t: not bad(t), bad)
    else:
        f2p_ok, f2p_bad = split(f2p, lambda t: passed(t, status_map), lambda t: failed(t, status_map))
        p2p_ok, p2p_bad = split(p2p, lambda t: maintained(t, status_map), lambda t: failed(t, status_map))
    resolved = not f2p_bad and not p2p_bad
    return {
        "valid": True,
        "resolved": resolved,
        "f2p": f"{len(f2p_ok)}/{len(f2p)}",
        "p2p": f"{len(p2p_ok)}/{len(p2p)}",
        "f2p_failed": f2p_bad[:5],
        "p2p_failed": p2p_bad[:5],
    }


def main(argv):
    if len(argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    with open(argv[1]) as f:
        instance = json.load(f)
    with open(argv[2], errors="replace") as f:
        content = f.read()
    report = grade(instance, content)
    print(json.dumps(report))
    if not report["valid"]:
        return 2
    return 0 if report["resolved"] else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
