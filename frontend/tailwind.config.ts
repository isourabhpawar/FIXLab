import type { Config } from "tailwindcss";

const config: Config = {
  content: ["./app/**/*.{ts,tsx}", "./components/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        background: "#0a0e14",
        surface: "#11161f",
        panel: "#161c28",
        border: "#232c3d",
        muted: "#8b94a7",
        accent: "#4cc38a",
        danger: "#f26d6d",
        warn: "#e5c07b",
        info: "#61afef",
      },
      fontFamily: {
        mono: ["ui-monospace", "SFMono-Regular", "Menlo", "Consolas", "monospace"],
      },
    },
  },
  plugins: [],
};

export default config;
