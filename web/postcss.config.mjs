// Tailwind v4 is a PostCSS plugin and needs no tailwind.config.js: the design tokens live in
// CSS via @theme (see src/app/globals.css). One fewer config file, and the tokens sit next to
// the styles that use them.
const config = { plugins: { "@tailwindcss/postcss": {} } };
export default config;
