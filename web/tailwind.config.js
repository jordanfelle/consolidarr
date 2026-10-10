/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  darkMode: "class",
  theme: {
    extend: {
      colors: {
        bg: "#0b0d12", panel: "#12151c", line: "#232834", muted: "#8b93a5",
        accent: "#6d8bff", good: "#34d399", warn: "#fbbf24", bad: "#f87171",
      },
    },
  },
  plugins: [],
};
