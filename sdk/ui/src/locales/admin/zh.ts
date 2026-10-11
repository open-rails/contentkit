import type { admin as en } from "./en.js";

export const admin: typeof en = {
  admin: {
    noAccessTitle: "无权访问",
    noAccess: "你没有使用此功能的权限。",
    pages: "分页",
    previous: "上一页",
    next: "下一页",
    page: "第 {page} 页",
  },
  standing: {
    title: "评论",
    noAccess: "你没有管理评论的权限。",
    ban: "全站评论封禁",
    notBanned: "未被封禁，可以评论。",
    banLoadFailed: "无法加载封禁信息。",
    comments: "最近的评论",
    noComments: "没有评论。",
  },
};
