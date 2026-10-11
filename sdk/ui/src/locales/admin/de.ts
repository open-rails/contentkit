import type { admin as en } from "./en.js";

export const admin: typeof en = {
  admin: {
    noAccessTitle: "Kein Zugriff",
    noAccess: "Du hast keine Berechtigung, dies zu verwenden.",
    pages: "Seiten",
    previous: "Zurück",
    next: "Weiter",
    page: "Seite {page}",
  },
  standing: {
    title: "Kommentieren",
    noAccess: "Du hast keine Berechtigung, Kommentare zu verwalten.",
    ban: "Kommentarsperre für die ganze Website",
    notBanned: "Nicht gesperrt: Diese Person darf kommentieren.",
    banLoadFailed: "Die Sperre konnte nicht geladen werden.",
    comments: "Neueste Kommentare",
    noComments: "Keine Kommentare.",
  },
};
