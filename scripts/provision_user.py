#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""新 API 用户开通脚本（管理员远程调用）。

流程：
  1. 查套餐：按标题找，没有就创建，有就复用
  2. 查用户：按用户 id（user_id，由业务系统提供）找，没有就创建
  3. 绑定套餐并发放 API Key，返回明文 key

依赖：pip install requests
运行：python provision_user.py --user-id 1001
"""

import argparse
import os
import sys

import requests

# ============ 配置 ============

# New API 地址
BASE_URL = os.environ.get("NEWAPI_BASE_URL", "http://localhost:3000")

# 管理员远程凭证（PAT，nap_...），建议用环境变量传入，不要写死在代码里
ADMIN_TOKEN = os.environ.get("NEWAPI_ADMIN_TOKEN", "nap_在此填入你的访问令牌")

# 要绑定的套餐（按 title 匹配；不存在则按下方 PLAN_SPEC 创建）
PLAN_TITLE = "日更套餐"

# 仅当套餐不存在时使用；total_amount 为每周期额度（quota 单位）
PLAN_SPEC = {
    "title": PLAN_TITLE,
    "subtitle": "",
    "price_amount": 0,
    "currency": "USD",
    "duration_unit": "year",
    "duration_value": 1,
    "custom_seconds": 0,
    "total_amount": 5000000,
    "quota_reset_period": "daily",
    "quota_reset_custom_seconds": 0,
    "upgrade_group": "",
    "downgrade_group": "",
    "max_purchase_per_user": 0,
    "enabled": True,
    "allow_balance_pay": True,
    "allow_wallet_overflow": True,
    "sort_order": 0,
}

# 待开通用户（业务系统提供）。user_id 为业务系统唯一 id，同时作为 New API 的用户主键。
USER = {
    "user_id": "1001",           # 业务系统唯一 id（New API 用户主键）
    "username": "stu1001",       # 可选；留空则用 user_id
    "email": "stu1001@example.com",
    "password": "",              # 留空则服务端随机生成 24 位
    "display_name": "学生1001",
}

# ==============================


class ApiError(RuntimeError):
    pass


def api(method, path, **kwargs):
    """调用管理接口，返回响应体中的 data 字段，失败抛 ApiError。"""
    headers = {"Authorization": f"Bearer {ADMIN_TOKEN}"}
    if kwargs.get("json") is not None:
        headers["Content-Type"] = "application/json"
    url = BASE_URL.rstrip("/") + path
    resp = requests.request(method, url, headers=headers, timeout=30, **kwargs)
    try:
        body = resp.json()
    except ValueError:
        raise ApiError(f"{method} {path} -> HTTP {resp.status_code}: {resp.text}")
    if resp.status_code >= 400 or not body.get("success"):
        raise ApiError(f"{method} {path} -> {body.get('message') or resp.text}")
    return body.get("data")


def find_plan_by_title(title):
    """返回标题匹配的套餐（含 id），找不到返回 None。"""
    plans = api("GET", "/api/subscription/admin/plans") or []
    for item in plans:
        plan = item.get("plan", item)
        if plan.get("title") == title:
            return plan
    return None


def ensure_plan():
    """套餐存在则复用，不存在则创建，返回 plan_id。"""
    plan = find_plan_by_title(PLAN_TITLE)
    if plan:
        print(f"[套餐] 已存在 id={plan['id']} title={PLAN_TITLE!r}，跳过创建")
        return plan["id"]
    print(f"[套餐] 不存在，创建 title={PLAN_TITLE!r}")
    created = api("POST", "/api/subscription/admin/plans", json={"plan": PLAN_SPEC})
    print(f"[套餐] 已创建 id={created['id']}")
    return created["id"]


def find_user_by_id(user_id):
    """按用户 id 查找用户，不存在返回 None。"""
    url = BASE_URL.rstrip("/") + f"/api/user/{user_id}"
    resp = requests.get(url, headers={"Authorization": f"Bearer {ADMIN_TOKEN}"}, timeout=30)
    try:
        body = resp.json()
    except ValueError:
        raise ApiError(f"GET /api/user/{user_id} -> HTTP {resp.status_code}: {resp.text}")
    if body.get("success"):
        return body["data"]
    message = str(body.get("message", ""))
    if "record not found" in message.lower() or "不存在" in message:
        return None
    raise ApiError(f"GET /api/user/{user_id} -> {message or resp.text}")


def provision(user_id, username, plan_id, email="", password="", display_name=""):
    """一站式：user 不存在则按该 id 创建，然后绑套餐并发 key，返回 data。

    对应 POST /api/user/provision（user_id 为 New API 用户主键）。
    """
    payload = {
        "user_id": int(user_id),
        "external_id": username or "",
        "email": email,
        "password": password,
        "display_name": display_name,
        "plan_id": plan_id,
    }
    return api("POST", "/api/user/provision", json=payload)


def main():
    parser = argparse.ArgumentParser(description="为新 API 开通用户并返回 API Key")
    parser.add_argument("--user-id", default=USER["user_id"], help="业务系统唯一 id（New API 用户主键）")
    parser.add_argument("--username", default=USER["username"], help="New API 登录名，留空则用 user-id")
    parser.add_argument("--email", default=USER["email"])
    parser.add_argument("--password", default=USER["password"])
    parser.add_argument("--display-name", default=USER["display_name"])
    args = parser.parse_args()

    if not args.user_id:
        print("错误：必须提供 --user-id", file=sys.stderr)
        return 1
    if ADMIN_TOKEN.startswith("nap_在此"):
        print("错误：请先在脚本顶部或环境变量 NEWAPI_ADMIN_TOKEN 填入管理员 PAT", file=sys.stderr)
        return 1

    plan_id = ensure_plan()

    user_id = int(args.user_id)
    existing = find_user_by_id(user_id)
    if existing:
        print(f"[用户] 已存在 id={existing['id']} username={existing['username']!r}")
    else:
        print(f"[用户] 不存在，将创建 id={user_id}")

    data = provision(user_id, args.username, plan_id, args.email, args.password, args.display_name)
    action = "已创建" if data.get("created") else "已复用"
    print(f"[用户] {action} user_id={data['user_id']} username={data['username']}")
    print(f"[套餐] 已绑定 plan_id={data.get('plan_id')} title={data.get('plan_title')!r}")
    print("[密钥] API Key（仅本次返回，请立即保存）：")
    print(data["key"])
    return 0


if __name__ == "__main__":
    sys.exit(main())
