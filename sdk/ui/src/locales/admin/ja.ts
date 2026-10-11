import type { admin as en } from "./en.js";

export const admin: typeof en = {
  admin: {
    noAccessTitle: "アクセス権がありません",
    noAccess: "この機能を使う権限がありません。",
    pages: "ページ",
    previous: "前へ",
    next: "次へ",
    page: "{page} ページ",
  },
  standing: {
    title: "コメント",
    noAccess: "コメントを管理する権限がありません。",
    ban: "サイト全体のコメント禁止",
    notBanned: "禁止されていません。コメントできます。",
    banLoadFailed: "禁止情報を読み込めませんでした。",
    comments: "最近のコメント",
    noComments: "コメントはありません。",
  },
};
