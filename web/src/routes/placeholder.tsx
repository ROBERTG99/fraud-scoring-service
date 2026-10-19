export default function Placeholder({ title }: { title: string }) {
  return (
    <section className="p-6">
      <h1 className="text-xl font-semibold">{title}</h1>
    </section>
  );
}
