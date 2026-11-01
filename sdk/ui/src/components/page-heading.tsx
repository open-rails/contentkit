import { createContext, useContext, type HTMLAttributes, type ReactNode } from "react";

const Nested = createContext(false);

/** Wraps managers shown inside a page that already has its own h1. */
export const NestedPageHeadings = ({ children }: { children: ReactNode }) => <Nested.Provider value>{children}</Nested.Provider>;

/** A manager's or editor's title: the page's h1, or an h2 under a host h1. */
export function PageHeading(props: HTMLAttributes<HTMLHeadingElement>) {
  const Tag = useContext(Nested) ? "h2" : "h1";
  return <Tag {...props} />;
}
