import type { admin as en } from "./en.js";

export const admin: typeof en = {
  admin: {
    noAccessTitle: "Sin acceso",
    noAccess: "No tienes permiso para usar esto.",
    pages: "Páginas",
    previous: "Anterior",
    next: "Siguiente",
    page: "Página {page}",
  },
  standing: {
    title: "Comentarios",
    noAccess: "No tienes permiso para gestionar comentarios.",
    ban: "Bloqueo de comentarios en todo el sitio",
    notBanned: "Sin bloqueo: puede comentar.",
    banLoadFailed: "No se pudo cargar el bloqueo.",
    comments: "Comentarios recientes",
    noComments: "No hay comentarios.",
  },
};
