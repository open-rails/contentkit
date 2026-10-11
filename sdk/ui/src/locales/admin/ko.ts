import type { admin as en } from "./en.js";

export const admin: typeof en = {
  admin: {
    noAccessTitle: "접근 권한 없음",
    noAccess: "이 기능을 사용할 권한이 없습니다.",
    pages: "페이지",
    previous: "이전",
    next: "다음",
    page: "{page}페이지",
  },
  standing: {
    title: "댓글",
    noAccess: "댓글을 관리할 권한이 없습니다.",
    ban: "사이트 전체 댓글 금지",
    notBanned: "금지되지 않았습니다. 댓글을 쓸 수 있습니다.",
    banLoadFailed: "금지 정보를 불러오지 못했습니다.",
    comments: "최근 댓글",
    noComments: "댓글이 없습니다.",
  },
};
