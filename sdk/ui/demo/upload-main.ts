// A bare page for driving the client from specs (page.evaluate): the
// signed-in session and the client factory on window.demo.
import { createContentKitClient } from "@openrails/contentkit-ui/client";
import { auth } from "./session";

const demo = { auth, createContentKitClient };
declare global {
  interface Window {
    demo: typeof demo;
  }
}
window.demo = demo;
document.body.dataset.ready = "true";
