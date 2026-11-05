// Local-only deterministic model for development. No external requests or credentials.
const http = require('node:http');
const port = Number(process.env.OPS_MOCK_PORT || 7799);
function reply(messages) {
  const lastUser = messages.findLastIndex((m) => m.role === 'user');
  const request = String(messages[lastUser]?.content || '');
  const results = messages.slice(lastUser + 1).filter((m) => m.role === 'tool');
  if (results.length) return { role: 'assistant', content: `本地模拟模型已收到工具结果。\n\n${results.map((m) => m.content).join('\n\n')}\n\n这是开发测试，不代表真实云端模型的判断。` };
  if (request.includes('Connection test')) return { role: 'assistant', content: 'OK' };
  let name;
  let args;
  if (request.includes('演示记忆')) {
    name = 'propose_memory';
    args = { text: '开发测试主机用于 Nginx 日志诊断；此记录为本地功能测试。', category: 'fact', subject: '开发主机' };
  } else if (request.includes('演示文件修改')) {
    name = 'write_file';
    args = { path: 'ops-demo.txt', content: 'Approved local development test.\n' };
  } else if (request.includes('列出') || request.includes('目录')) {
    name = 'list_directory';
    args = { limit: 10 };
  } else if (request.includes('状态') || request.includes('检查')) {
    name = 'system_status';
    args = {};
  }
  if (name) return { role: 'assistant', content: `本地模拟模型准备调用 ${name}。需要审批的操作会等待你确认。`, tool_calls: [{ id: `demo_${Date.now()}`, type: 'function', function: { name, arguments: JSON.stringify(args) } }] };
  return { role: 'assistant', content: '本地模拟模型已连接。可以输入“检查状态”“列出测试目录”“演示记忆”或“演示文件修改”验证工具、记忆和审批。所有结果来自本机开发测试，不会发送到外部模型。' };
}
const server = http.createServer((req, res) => {
  if (req.method !== 'POST' || req.url !== '/chat/completions') { res.writeHead(404); res.end(); return; }
  let body = '';
  req.on('data', (chunk) => { body += chunk; if (body.length > 1024 * 1024) req.destroy(); });
  req.on('end', () => {
    try {
      const data = JSON.parse(body);
      const message = reply(Array.isArray(data.messages) ? data.messages : []);
      res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' });
      res.write(`data: ${JSON.stringify({ choices: [{ delta: { role: 'assistant', content: message.content } }] })}\n\n`);
      for (const [index, call] of (message.tool_calls || []).entries()) res.write(`data: ${JSON.stringify({ choices: [{ delta: { tool_calls: [{ index, ...call }] } }] })}\n\n`);
      res.write(`data: ${JSON.stringify({ choices: [{ delta: {}, finish_reason: message.tool_calls ? 'tool_calls' : 'stop' }] })}\n\n`);
      res.end('data: [DONE]\n\n');
    } catch { res.writeHead(400); res.end('Invalid development request'); }
  });
});
server.listen(port, '127.0.0.1', () => console.log(`Local development model: http://127.0.0.1:${port}`));
process.on('SIGINT', () => server.close());
process.on('SIGTERM', () => server.close());
