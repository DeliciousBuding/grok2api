#!/usr/bin/env python3
"""
grok2api 聊天接口测试脚本
支持：普通请求 / 流式请求 / 图片请求 / 多轮对话
"""

import json
import sys
import time
import urllib.request
import urllib.error


# ==================== 配置 ====================
BASE_URL = "http://127.0.0.1:8000"
API_KEY = ""                     # 空 = 不鉴权
MODEL = "grok-4.20-auto"         # 模型名称
STREAM = False                   # 是否流式输出
TIMEOUT = 300                    # 请求超时(秒)


# ==================== 辅助函数 ====================

def build_headers():
    headers = {"Content-Type": "application/json"}
    if API_KEY:
        headers["Authorization"] = f"Bearer {API_KEY}"
    return headers


def send_request(messages: list, model: str = MODEL, stream: bool = STREAM,
                 temperature: float = 0.8, max_tokens: int = None,
                 reasoning_effort: str = None) -> dict:
    """发送聊天请求并返回完整响应（非流式）。"""
    body = {
        "model": model,
        "messages": messages,
        "stream": False,
        "temperature": temperature,
    }
    if max_tokens is not None:
        body["max_tokens"] = max_tokens
    if reasoning_effort is not None:
        body["reasoning_effort"] = reasoning_effort

    data = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(
        f"{BASE_URL}/v1/chat/completions",
        data=data,
        headers=build_headers(),
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        body_text = e.read().decode("utf-8", errors="replace")
        print(f"[错误] HTTP {e.code}: {body_text}")
        sys.exit(1)
    except Exception as e:
        print(f"[错误] {e}")
        sys.exit(1)


def send_request_stream(messages: list, model: str = MODEL,
                        temperature: float = 0.8,
                        reasoning_effort: str = None):
    """发送流式请求，逐块 yield 解析后的 dict。"""
    from http.client import HTTPResponse

    body = {
        "model": model,
        "messages": messages,
        "stream": True,
        "temperature": temperature,
    }
    if reasoning_effort is not None:
        body["reasoning_effort"] = reasoning_effort

    data = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(
        f"{BASE_URL}/v1/chat/completions",
        data=data,
        headers=build_headers(),
        method="POST",
    )
    try:
        resp: HTTPResponse = urllib.request.urlopen(req, timeout=TIMEOUT)
    except urllib.error.HTTPError as e:
        body_text = e.read().decode("utf-8", errors="replace")
        print(f"[错误] HTTP {e.code}: {body_text}")
        sys.exit(1)
    except Exception as e:
        print(f"[错误] {e}")
        sys.exit(1)

    buffer = ""
    while True:
        chunk = resp.read(4096)
        if not chunk:
            break
        buffer += chunk.decode("utf-8", errors="replace")
        # SSE 解析
        while "\n\n" in buffer:
            block, buffer = buffer.split("\n\n", 1)
            for line in block.split("\n"):
                if line.startswith("data: "):
                    payload = line[6:].strip()
                    if payload == "[DONE]":
                        return
                    try:
                        yield json.loads(payload)
                    except json.JSONDecodeError:
                        print(f"[parse error] {payload}")


def extract_content(response: dict) -> str:
    """从响应 dict 中提取助手回复文本。"""
    try:
        return response["choices"][0]["message"]["content"]
    except (KeyError, IndexError):
        return ""


def extract_reasoning(response: dict) -> str:
    """提取 thinking / reasoning_content（如果存在）。"""
    try:
        msg = response["choices"][0]["message"]
        return msg.get("reasoning_content") or msg.get("reasoning") or ""
    except (KeyError, IndexError):
        return ""


# ==================== 测试用例 ====================

def test_simple_chat():
    """测试 1：简单问答"""
    print("=" * 60)
    print("测试 1：简单问答")
    print("=" * 60)
    messages = [
        {"role": "user", "content": "用一句话解释量子计算是什么"}
    ]
    resp = send_request(messages)
    content = extract_content(resp)
    print(f"模型: {resp.get('model', '?')}")
    print(f"回答: {content[:300]}{'...' if len(content) > 300 else ''}")
    print()
    return resp


def test_streaming_chat():
    """测试 2：流式请求"""
    print("=" * 60)
    print("测试 2：流式请求（逐块输出）")
    print("=" * 60)
    messages = [
        {"role": "user", "content": "从 1 数到 5，每个数字一行"}
    ]
    full_text = ""
    reasoning_text = ""
    for chunk in send_request_stream(messages):
        choices = chunk.get("choices", [])
        if not choices:
            continue
        delta = choices[0].get("delta", {})
        if "reasoning_content" in delta:
            t = delta["reasoning_content"]
            reasoning_text += t
            print(f"[思考] {t}", end="", flush=True)
        if "content" in delta:
            t = delta["content"]
            full_text += t
            print(t, end="", flush=True)
    print(f"\n\n[完成] 全文长度: {len(full_text)}")
    print()


def test_multi_turn():
    """测试 3：多轮对话"""
    print("=" * 60)
    print("测试 3：多轮对话")
    print("=" * 60)
    messages = [
        {"role": "system", "content": "你是一个只说一句话的助手。"},
        {"role": "user", "content": "天空是什么颜色的？"},
        {"role": "assistant", "content": "蓝色。"},
        {"role": "user", "content": "为什么？"}
    ]
    resp = send_request(messages)
    content = extract_content(resp)
    print(f"回答: {content}")
    print()


def test_reasoning_effort():
    """测试 4：启用 thinking / reasoning_effort"""
    print("=" * 60)
    print("测试 4：启用 thinking / reasoning_effort")
    print("=" * 60)
    messages = [
        {"role": "user", "content": "证明根号2是无理数"}
    ]
    resp = send_request(messages, reasoning_effort="high")
    content = extract_content(resp)
    reasoning = extract_reasoning(resp)
    print(f"回答: {content[:200]}{'...' if len(content) > 200 else ''}")
    if reasoning:
        print(f"思考过程: {reasoning[:200]}{'...' if len(reasoning) > 200 else ''}")
    else:
        print("(无 thinking 输出)")
    print()


def test_system_prompt():
    """测试 5：System Prompt"""
    print("=" * 60)
    print("测试 5：System Prompt")
    print("=" * 60)
    messages = [
        {"role": "system", "content": "你是一个猫咪助手，所有回答都在末尾加上「喵~」。"},
        {"role": "user", "content": "今天天气怎么样？"}
    ]
    resp = send_request(messages)
    content = extract_content(resp)
    print(f"回答: {content[:200]}{'...' if len(content) > 200 else ''}")
    print()


def test_image_content():
    """测试 6：图片输入（多模态，发送图片 URL）"""
    print("=" * 60)
    print("测试 6：图片输入（多模态）")
    print("=" * 60)
    messages = [
        {
            "role": "user",
            "content": [
                {"type": "text", "text": "描述这张图片的内容"},
                {
                    "type": "image_url",
                    "image_url": {
                        "url": "https://upload.wikimedia.org/wikipedia/commons/thumb/4/47/PNG_transparency_demonstration_1.png/300px-PNG_transparency_demonstration_1.png"
                    }
                },
            ]
        }
    ]
    resp = send_request(messages)
    content = extract_content(resp)
    print(f"回答: {content[:300]}{'...' if len(content) > 300 else ''}")
    print()


def test_health():
    """辅助：检测服务是否存活"""
    print("=" * 60)
    print("健康检查")
    print("=" * 60)
    try:
        req = urllib.request.Request(f"{BASE_URL}/health")
        with urllib.request.urlopen(req, timeout=5) as resp:
            data = json.loads(resp.read().decode("utf-8"))
            print(f"状态: {data}")
            return True
    except Exception as e:
        print(f"服务不可达: {e}")
        return False


def test_model_list():
    """辅助：列出可用模型"""
    print("=" * 60)
    print("可用模型列表")
    print("=" * 60)
    try:
        req = urllib.request.Request(
            f"{BASE_URL}/v1/models",
            headers=build_headers(),
        )
        with urllib.request.urlopen(req, timeout=10) as resp:
            data = json.loads(resp.read().decode("utf-8"))
            for m in data.get("data", []):
                print(f"  - {m.get('id', '?')}")
        print()
        return True
    except Exception as e:
        print(f"获取失败: {e}")
        return False


# ==================== 主入口 ====================

def main():
    print(f"grok2api 聊天测试脚本")
    print(f"目标: {BASE_URL}")
    print(f"模型: {MODEL}")
    print()

    # 0. 健康检查
    if not test_health():
        print("请确认服务已启动后再运行。")
        sys.exit(1)

    # 0.5 列出模型
    test_model_list()

    # 1. 简单问答
    test_simple_chat()

    # 2. 流式请求 (如果启用)
    if STREAM:
        test_streaming_chat()

    # 3. 系统指令
    test_system_prompt()

    # 4. 多轮对话
    test_multi_turn()

    # 5. Reasoning effort
    test_reasoning_effort()

    # 6. 图片输入
    test_image_content()

    print("所有测试完成。")


if __name__ == "__main__":
    main()
