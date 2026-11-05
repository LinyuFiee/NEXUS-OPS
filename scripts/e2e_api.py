"""Local preview API regression; uses only the explicitly supplied test directory."""
import argparse
import http.cookiejar
import json
import pathlib
import sqlite3
import time
import urllib.error
import urllib.request

p = argparse.ArgumentParser()
p.add_argument('--base', default='http://127.0.0.1:7788')
p.add_argument('--data-dir', required=True)
p.add_argument('--work-dir', required=True)
a = p.parse_args()
if not a.base.startswith('http://127.0.0.1:'):
    raise SystemExit('Only loopback preview servers are allowed')
with sqlite3.connect(str(pathlib.Path(a.data_dir) / 'ops.db')) as db:
    token = db.execute("SELECT value FROM kv WHERE key='access_token'").fetchone()[0]
jar = http.cookiejar.CookieJar()
client = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))

def req(method, path, payload=None, authenticated=True, headers=None):
    data = None if payload is None else json.dumps(payload).encode()
    hs = {'Content-Type': 'application/json', 'X-Ops-Request': '1'}
    hs.update(headers or {})
    request = urllib.request.Request(a.base + path, data=data, headers=hs, method=method)
    opener = client if authenticated else urllib.request.build_opener()
    try:
        with opener.open(request, timeout=15) as res:
            return res.status, json.loads(res.read() or b'null')
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read() or b'null')

checks = []
def check(condition, label):
    if not condition:
        raise AssertionError(label)
    checks.append(label)
    print('PASS', label)

check(req('GET', '/api/tasks', authenticated=False)[0] == 401, 'unauthenticated task API rejected')
check(req('GET', '/api/events', authenticated=False)[0] == 401, 'unauthenticated SSE rejected')
check(req('POST', '/api/login', {'token': token})[0] == 200, 'token login')
check(req('GET', '/api/setup/config')[0] == 401, 'setup stays closed after initialization')
check(req('POST', '/api/tasks', {'message': 'test'}, headers={'Origin': 'https://untrusted.example'})[0] == 403, 'cross-origin mutation rejected')
check(req('POST', '/api/test/model', {'model': {'base_url': 'https://untrusted.example', 'name': 'mock'}, 'api_key': ''})[0] == 400, 'saved model key not sent to a changed endpoint')


def start(message):
    code, task = req('POST', '/api/tasks', {'message': message})
    check(code == 202, 'task submitted: ' + message)
    return task


def await_approval(task):
    deadline = time.monotonic() + 12
    while time.monotonic() < deadline:
        code, approvals = req('GET', '/api/approvals')
        for approval in approvals:
            if approval['task_id'] == task['id']:
                return approval
        time.sleep(0.04)
    raise AssertionError('approval not generated')


def await_finished(task):
    deadline = time.monotonic() + 12
    while time.monotonic() < deadline:
        _, tasks = req('GET', '/api/tasks')
        latest = next(x for x in tasks if x['id'] == task['id'])
        if latest['status'] not in ('queued', 'running', 'waiting_approval'):
            return latest
        time.sleep(0.04)
    raise AssertionError('task did not finish')

path = pathlib.Path(a.work_dir) / 'ops-demo.txt'
if path.exists():
    if path.read_text() != 'Approved local development test.\n':
        raise AssertionError('Refusing to replace non-test file')
    path.unlink()
check(not path.exists(), 'test output path initially absent')
task = start('演示文件修改')
approval = await_approval(task)
check(not path.exists(), 'file not created before approval')
check(req('POST', '/api/approvals/' + approval['id'], {'approve': False})[0] == 200, 'file change declined')
check(await_finished(task)['status'] == 'completed', 'decline returned to model without executing')
check(not path.exists(), 'declined file change has no filesystem side effect')
check(req('POST', '/api/approvals/' + approval['id'], {'approve': True})[0] == 409, 'used approval cannot be replayed')

task = start('演示文件修改')
approval = await_approval(task)
check(not path.exists(), 'second file proposal also waits for approval')
check(req('POST', '/api/approvals/' + approval['id'], {'approve': True})[0] == 200, 'specific file change approved')
finished = await_finished(task)
check(finished['status'] == 'completed', 'approved file tool and second model turn completed')
check(path.read_text() == 'Approved local development test.\n', 'approved file contents verified')

_, memories_before = req('GET', '/api/memories')
task = start('演示记忆')
approval = await_approval(task)
check(req('POST', '/api/tasks/' + task['id'] + '/cancel', {})[0] == 200, 'waiting task cancelled')
check(await_finished(task)['status'] == 'cancelled', 'cancelled state persisted')
_, memories_after = req('GET', '/api/memories')
check(len(memories_after) == len(memories_before), 'cancelled memory proposal never saved')
check(req('POST', '/api/approvals/' + approval['id'], {'approve': True})[0] == 409, 'cancelled approval expired')

_, memories = req('GET', '/api/memories')
check(any('开发测试主机' in item['text'] for item in memories), 'browser-approved memory persisted')
_, hits = req('GET', '/api/memories/search?q=Nginx')
check(bool(hits), 'Chinese/English memory search returns stored evidence')
check(req('POST', '/api/logout', {})[0] == 200, 'logout')
check(req('GET', '/api/tasks')[0] == 401, 'logged-out cookie rejected')
print('RESULT', len(checks), 'local API checks passed; no real cloud model or QQ messages sent')
