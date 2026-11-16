// The poll staff words of every shipped language, read from the SDK sources.
const out = {};
for (const lang of ["en", "de", "es", "ja", "ko", "zh"]) {
  const { admin } = await import(new URL(`../../sdk/ui/src/locales/admin/${lang}.ts`, import.meta.url));
  const { social } = await import(new URL(`../../sdk/ui/src/locales/social/${lang}.ts`, import.meta.url));
  out[lang] = { manager: admin.pollManager, editor: social.pollEditor };
}
console.log(JSON.stringify(out));
